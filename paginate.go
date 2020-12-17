package paginate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/iancoleman/strcase"
	"gorm.io/gorm"

	"github.com/valyala/fasthttp"
)

// Pagination struct
type Pagination struct {
	Config *Config
}

// Response func
func (p *Pagination) Response(query *gorm.DB, req interface{}, res interface{}) Page {
	if nil == p.Config {
		p.Config = &Config{}
	}
	p.Config.Statement = query.Statement
	if p.Config.DefaultSize == 0 {
		p.Config.DefaultSize = 10
	}

	if p.Config.FieldWrapper == "" && p.Config.ValueWrapper == "" {
		switch query.Dialector.Name() {
		case "postgres":
			p.Config.FieldWrapper = "LOWER((%s)::text)"
		case "sqlite":
		case "mysql":
			p.Config.FieldWrapper = "LOWER(%s)"
		default:
			p.Config.FieldWrapper = "LOWER(%s)"
		}
	}

	page := Page{}
	pr := parseRequest(req, *p.Config)
	causes := createCauses(pr)
	dbs := query.Statement.DB.Session(&gorm.Session{NewDB: true})

	result := dbs.
		Unscoped().
		Table("(?) AS s", query)

	if len(causes.Params) > 0 {
		result = result.Where(causes.WhereString, causes.Params...)
	}

	result = result.Count(&page.Total).
		Limit(causes.Limit).
		Offset(causes.Offset)

	if nil != query.Statement.Preloads {
		for table, args := range query.Statement.Preloads {
			result = result.Preload(table, args...)
		}
	}
	if len(causes.Sorts) > 0 {
		for _, sort := range causes.Sorts {
			result = result.Order(sort.Column + " " + sort.Direction)
		}
	}

	result = result.Find(res)

	page.Items = res
	f := float64(page.Total) / float64(causes.Limit)
	if math.Mod(f, 1.0) > 0 {
		f = f + 1
	}
	page.TotalPages = int64(f)
	page.Page = int64(pr.Page)
	page.Size = int64(pr.Size)
	page.MaxPage = 0
	page.Visible = result.RowsAffected
	if page.TotalPages > 0 {
		page.MaxPage = page.TotalPages - 1
	}
	page.Fisrt = causes.Offset < 1
	page.Last = page.MaxPage == page.Page

	return page
}

// New func
func New(params ...interface{}) *Pagination {
	if len(params) >= 1 {
		var config *Config
		for _, param := range params {
			c, isConfig := param.(*Config)
			if isConfig {
				config = c
				continue
			}
		}

		if nil == config {
			e := errors.New("Invalid argument, first argument of paginate.New() must be instance of *paginate.Config")
			log.Println(e)
		}

		return &Pagination{Config: config}
	}

	return &Pagination{Config: &Config{}}
}

// parseRequest func
func parseRequest(r interface{}, config Config) pageRequest {
	pr := pageRequest{
		Config: config,
	}
	netHTTP, isNetHTTP := r.(http.Request)
	if isNetHTTP {
		parsingNetHTTPRequest(&netHTTP, &pr)
	} else {
		netHTTPp, isNetHTTPp := r.(*http.Request)
		if isNetHTTPp {
			parsingNetHTTPRequest(netHTTPp, &pr)
		} else {
			fastHTTP, isFastHTTP := r.(fasthttp.Request)
			if isFastHTTP {
				parsingFastHTTPRequest(&fastHTTP, &pr)
			} else {
				fastHTTPp, isFastHTTPp := r.(*fasthttp.Request)
				if isFastHTTPp {
					parsingFastHTTPRequest(fastHTTPp, &pr)
				}
			}
		}
	}

	return pr
}

// createFilters func
func createFilters(filterParams interface{}, p *pageRequest) {
	f, ok := filterParams.([]interface{})
	s, ok2 := filterParams.(string)
	if ok {
		p.Filters = arrayToFilter(f, p.Config)
	} else if ok2 {
		iface := []interface{}{}
		e := json.Unmarshal([]byte(s), &iface)
		if nil == e && len(iface) > 0 {
			p.Filters = arrayToFilter(iface, p.Config)
		}
	}
}

// createCauses func
func createCauses(p pageRequest) requestQuery {
	query := requestQuery{}
	wheres, params := generateWhereCauses(p.Filters, p.Config)
	sorts := []sortOrder{}

	for _, so := range p.Sorts {
		so.Column = fieldName(so.Column)
		if nil != p.Config.Statement {
			so.Column = p.Config.Statement.Quote(so.Column)
		}
		sorts = append(sorts, so)
	}

	query.Limit = p.Size
	query.Offset = p.Page * p.Size
	query.Wheres = wheres
	query.WhereString = strings.Join(wheres, " ")
	query.Sorts = sorts
	query.Params = params

	return query
}

// parsingNetHTTPRequest func
func parsingNetHTTPRequest(r *http.Request, p *pageRequest) {
	param := &parameter{}
	if r.Method == "" {
		r.Method = "GET"
	}
	if strings.ToUpper(r.Method) == "POST" {
		decoder := json.NewDecoder(r.Body)
		var postData map[string]string
		// var postData parameter
		if err := decoder.Decode(&postData); nil == err {
			generateParams(param, p.Config, func(key string) string {
				value, _ := postData[key]
				return value
			})
			// param = &postData
		} else {
			log.Println(err.Error())
		}
	} else if strings.ToUpper(r.Method) == "GET" {
		query := r.URL.Query()
		// param.Size = query.Get("size")
		// param.Page = query.Get("page")
		// param.Sort = query.Get("sort")
		// param.Order = query.Get("order")
		// param.Filters = query.Get("filters")
		generateParams(param, p.Config, func(key string) string {
			return query.Get(key)
		})
	}

	parsingQueryString(param, p)
}

// parsingFastHTTPRequest func
func parsingFastHTTPRequest(r *fasthttp.Request, p *pageRequest) {
	param := &parameter{}
	if r.Header.IsPost() {
		b := r.Body()
		var postData map[string]string
		// var postData parameter
		if err := json.Unmarshal(b, &postData); nil == err {
			generateParams(param, p.Config, func(key string) string {
				value, _ := postData[key]
				return value
			})
			// param = &postData
		} else {
			log.Println(err.Error())
		}
	} else if r.Header.IsGet() {
		query := r.URI().QueryArgs()
		// param.Size = string(query.Peek("size"))
		// param.Page = string(query.Peek("page"))
		// param.Sort = string(query.Peek("sort"))
		// param.Order = string(query.Peek("order"))
		// param.Filters = string(query.Peek("filters"))
		generateParams(param, p.Config, func(key string) string {
			return string(query.Peek(key))
		})
	}

	parsingQueryString(param, p)
}

func parsingQueryString(param *parameter, p *pageRequest) {
	if i, e := strconv.Atoi(param.Size); nil == e {
		p.Size = i
	} else if p.Config.DefaultSize > 0 {
		p.Size = int(p.Config.DefaultSize)
	} else {
		p.Size = 10
	}

	if i, e := strconv.Atoi(param.Page); nil == e {
		p.Page = i
	} else {
		p.Page = 0
	}

	if param.Sort != "" {
		sorts := strings.Split(param.Sort, ",")
		for _, col := range sorts {
			if col == "" {
				continue
			}

			so := sortOrder{
				Column:    col,
				Direction: "ASC",
			}
			if strings.ToUpper(param.Order) == "DESC" {
				so.Direction = "DESC"
			}

			if string(col[0]) == "-" {
				so.Column = string(col[1:])
				so.Direction = "DESC"
			}

			p.Sorts = append(p.Sorts, so)
		}
	}

	createFilters(param.Filters, p)
}

func generateParams(param *parameter, config Config, getValue func(string) string) {
	findValue := func(keys []string, defaultKey string) string {
		found := false
		value := ""
		for _, key := range keys {
			value = getValue(key)
			if value != "" {
				found = true
				break
			}
		}
		if !found {
			return getValue(defaultKey)
		}
		return value
	}

	param.Sort = findValue(config.SortParams, "sort")
	param.Page = findValue(config.SortParams, "page")
	param.Size = findValue(config.SortParams, "size")
	param.Order = findValue(config.SortParams, "order")
	param.Filters = findValue(config.SortParams, "filters")
}

func arrayToFilter(arr []interface{}, config Config) pageFilters {
	filters := pageFilters{
		Single: false,
	}

	operatorEscape := regexp.MustCompile(`[^A-z=\<\>\-\+\^/\*%& ]+`)
	arrayLen := len(arr)

	if len(arr) > 0 {
		subFilters := []pageFilters{}
		for k, i := range arr {
			iface, ok := i.([]interface{})
			if ok && !filters.Single {
				subFilters = append(subFilters, arrayToFilter(iface, config))
			} else if arrayLen == 1 {
				operator, ok := i.(string)
				if ok {
					operator = operatorEscape.ReplaceAllString(operator, "")
					filters.Operator = strings.ToUpper(operator)
					filters.IsOperator = true
					filters.Single = true
				}
			} else if arrayLen == 2 {
				if k == 0 {
					column, ok := i.(string)
					if ok {
						filters.Column = column
						filters.Operator = "="
						filters.Single = true
					}
				} else if k == 1 {
					filters.Value = i
				}
			} else if arrayLen == 3 {
				if k == 0 {
					column, ok := i.(string)
					if ok {
						filters.Column = column
						filters.Single = true
					}
				} else if k == 1 {
					operator, ok := i.(string)
					if ok {
						operator = operatorEscape.ReplaceAllString(operator, "")
						filters.Operator = strings.ToUpper(operator)
						filters.Single = true
					}
				} else if k == 2 {
					switch filters.Operator {
					case "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
						escapeString := ""
						if nil != config.Statement {
							driverName := config.Statement.Dialector.Name()
							switch driverName {
							case "sqlite", "mysql", "postgres":
								escapeString = `\`
								filters.ValueSuffix = "ESCAPE '\\'"
							}
						}
						value := fmt.Sprintf("%v", i)
						value = strings.ReplaceAll(value, "%", escapeString+"%")
						if config.SmartSearch {
							re := regexp.MustCompile(`[\s]+`)
							byt := re.ReplaceAll([]byte(value), []byte("%"))
							value = string(byt)
						}
						filters.Value = fmt.Sprintf("%s%s%s", "%", value, "%")
					default:
						filters.Value = i
					}
				}
			}
		}
		if len(subFilters) > 0 {
			separatedSubFilters := []pageFilters{}
			hasOperator := false
			defaultOperator := config.Operator
			if "" == defaultOperator {
				defaultOperator = "OR"
			}
			for k, s := range subFilters {
				if s.IsOperator && len(subFilters) == (k+1) {
					break
				}
				if !hasOperator && !s.IsOperator && k > 0 {
					separatedSubFilters = append(separatedSubFilters, pageFilters{
						Operator:   defaultOperator,
						IsOperator: true,
						Single:     true,
					})
				}
				hasOperator = s.IsOperator
				separatedSubFilters = append(separatedSubFilters, s)
			}
			filters.Value = separatedSubFilters
			filters.Single = false
		}
	}

	return filters
}

func generateWhereCauses(f pageFilters, config Config) ([]string, []interface{}) {
	wheres := []string{}
	params := []interface{}{}

	if !f.Single && !f.IsOperator {
		ifaces, ok := f.Value.([]pageFilters)
		if ok && len(ifaces) > 0 {
			wheres = append(wheres, "(")
			hasOpen := false
			for _, i := range ifaces {
				subs, isSub := i.Value.([]pageFilters)
				regular, isNotSub := i.Value.(pageFilters)
				if isSub && len(subs) > 0 {
					wheres = append(wheres, "(")
					for _, s := range subs {
						subWheres, subParams := generateWhereCauses(s, config)
						wheres = append(wheres, subWheres...)
						params = append(params, subParams...)
					}
					wheres = append(wheres, ")")
				} else if isNotSub {
					subWheres, subParams := generateWhereCauses(regular, config)
					wheres = append(wheres, subWheres...)
					params = append(params, subParams...)
				} else {
					if !hasOpen && !i.IsOperator {
						wheres = append(wheres, "(")
						hasOpen = true
					}
					subWheres, subParams := generateWhereCauses(i, config)
					wheres = append(wheres, subWheres...)
					params = append(params, subParams...)
				}
			}
			if hasOpen {
				wheres = append(wheres, ")")
			}
			wheres = append(wheres, ")")
		}
	} else if f.Single {
		if f.IsOperator {
			wheres = append(wheres, f.Operator)
		} else {
			fname := fieldName(f.Column)
			if nil != config.Statement {
				fname = config.Statement.Quote(fname)
			}
			switch f.Operator {
			case "IS", "IS NOT":
				isNull := f.Value == nil
				if isNull {
					wheres = append(wheres, fname, f.Operator, "NULL")
				} else {
					wheres = append(wheres, fname, f.Operator, "?")
					params = append(params, f.Value)
				}
			case "BETWEEN":
				values, ok := f.Value.([]interface{})
				if ok && len(values) >= 2 {
					wheres = append(wheres, "(", fname, f.Operator, "? AND ?", ")")
					params = append(params, values[0], values[1])
				}
			case "IN", "NOT IN":
				values, ok := f.Value.([]interface{})
				if ok {
					wheres = append(wheres, fname, f.Operator, "?")
					params = append(params, values)
				}
			case "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE":
				if config.FieldWrapper != "" {
					fname = fmt.Sprintf(config.FieldWrapper, fname)
				}
				wheres = append(wheres, fname, f.Operator, "?")
				if f.ValueSuffix != "" {
					wheres = append(wheres, f.ValueSuffix)
				}
				value, isStrValue := f.Value.(string)
				if isStrValue {
					if config.ValueWrapper != "" {
						value = fmt.Sprintf(config.ValueWrapper, value)
					} else {
						value = strings.ToLower(value)
					}
					params = append(params, value)
				} else {
					params = append(params, f.Value)
				}
			default:
				wheres = append(wheres, fname, f.Operator, "?")
				params = append(params, f.Value)
			}
		}
	}

	return wheres, params
}

func fieldName(field string) string {
	slices := strings.Split(field, ".")
	if len(slices) == 1 {
		return field
	}
	newSlices := []string{}
	if len(slices) > 0 {
		newSlices = append(newSlices, strcase.ToCamel(slices[0]))
		for k, s := range slices {
			if k > 0 {
				newSlices = append(newSlices, s)
			}
		}
	}
	if len(newSlices) == 0 {
		return field
	}
	return strings.Join(newSlices, "__")

}

// Config struct
type Config struct {
	Operator     string
	FieldWrapper string
	ValueWrapper string
	DefaultSize  int64
	SmartSearch  bool
	Statement    *gorm.Statement `json:"-"`
	SortParams   []string
	PageParams   []string
	OrderParams  []string
	SizeParams   []string
	FilterParams []string
}

// pageFilters struct
type pageFilters struct {
	Column      string
	Operator    string
	Value       interface{}
	ValuePrefix string
	ValueSuffix string
	Single      bool
	IsOperator  bool
}

// Page struct
type Page struct {
	Items      interface{} `json:"items"`
	Page       int64       `json:"page"`
	Size       int64       `json:"size"`
	MaxPage    int64       `json:"max_page"`
	TotalPages int64       `json:"total_pages"`
	Total      int64       `json:"total"`
	Last       bool        `json:"last"`
	Fisrt      bool        `json:"first"`
	Visible    int64       `json:"visible"`
}

// parameter struct
type parameter struct {
	Page    string      `json:"page"`
	Size    string      `json:"size"`
	Sort    string      `json:"sort"`
	Order   string      `json:"order"`
	Filters interface{} `json:"filters"`
}

// query struct
type requestQuery struct {
	WhereString string
	Wheres      []string
	Params      []interface{}
	Sorts       []sortOrder
	Limit       int
	Offset      int
}

// pageRequest struct
type pageRequest struct {
	Size    int
	Page    int
	Sorts   []sortOrder
	Filters pageFilters
	Config  Config `json:"-"`
}

// SortOrder struct
type sortOrder struct {
	Column    string
	Direction string
}
