package paginate

import (
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/iancoleman/strcase"
	"github.com/morkid/gocache"
	"gorm.io/gorm"

	"github.com/valyala/fasthttp"
)

// ResponseContext interface
type ResponseContext interface {
	Cache(string) ResponseContext
	Fields([]string) ResponseContext
	Response(interface{}) Page
}

// RequestContext interface
type RequestContext interface {
	Request(interface{}) ResponseContext
}

// Pagination gorm paginate struct
type Pagination struct {
	Config *Config
}

// Response return page of results
//
// Deprecated: Response must not be used. Use With instead
func (p *Pagination) Response(stmt *gorm.DB, req interface{}, res interface{}) Page {
	return p.With(stmt).Request(req).Response(res)
}

// With func
func (p *Pagination) With(stmt *gorm.DB) RequestContext {
	return reqContext{
		Statement:  stmt,
		Pagination: p,
	}
}

// ClearCache clear cache contains prefix
func (p Pagination) ClearCache(keyPrefixes ...string) {
	if len(keyPrefixes) > 0 && nil != p.Config && nil != p.Config.CacheAdapter {
		adapter := *p.Config.CacheAdapter
		for i := range keyPrefixes {
			go func(adapter gocache.AdapterInterface, keyPrefix string) {
				if err := adapter.ClearPrefix(keyPrefix); nil != err {
					log.Println(err)
				}
			}(adapter, keyPrefixes[i])
		}
	}
}

// ClearAllCache clear all existing cache
func (p Pagination) ClearAllCache() {
	if nil != p.Config && nil != p.Config.CacheAdapter {
		adapter := *p.Config.CacheAdapter
		go func(adapter gocache.AdapterInterface) {
			if err := adapter.ClearAll(); nil != err {
				log.Println(err)
			}
		}(adapter)
	}
}

type reqContext struct {
	Statement  *gorm.DB
	Pagination *Pagination
}

func (r reqContext) Request(req interface{}) ResponseContext {
	var response ResponseContext = &resContext{
		Statement:  r.Statement,
		Request:    req,
		Pagination: r.Pagination,
	}

	return response
}

type resContext struct {
	Pagination  *Pagination
	Statement   *gorm.DB
	Request     interface{}
	cachePrefix string
	fieldList   []string
}

func (r *resContext) Cache(prefix string) ResponseContext {
	r.cachePrefix = prefix
	return r
}

func (r *resContext) Fields(fields []string) ResponseContext {
	r.fieldList = fields
	return r
}

func (r resContext) Response(res interface{}) Page {
	p := r.Pagination
	query := r.Statement
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
		case "sqlite", "mysql":
			p.Config.FieldWrapper = "LOWER(%s)"
		default:
			p.Config.FieldWrapper = "LOWER(%s)"
		}
	}

	page := Page{}
	pr := parseRequest(r.Request, *p.Config)
	causes := createCauses(pr)
	cKey := ""
	var adapter gocache.AdapterInterface
	var hasAdapter bool = false

	if nil != p.Config.CacheAdapter {
		cKey = createCacheKey(r.cachePrefix, pr)
		adapter = *p.Config.CacheAdapter
		hasAdapter = true
		if cKey != "" && adapter.IsValid(cKey) {
			if cache, err := adapter.Get(cKey); nil == err {
				page.Items = res
				if err := json.Unmarshal([]byte(cache), &page); nil == err {
					return page
				}
			}
		}
	}

	dbs := query.Statement.DB.Session(&gorm.Session{NewDB: true})
	var selects []string
	if len(r.fieldList) > 0 {
		if len(pr.Fields) > 0 && p.Config.FieldSelectorEnabled {
			for i := range pr.Fields {
				for j := range r.fieldList {
					if r.fieldList[j] == pr.Fields[i] {
						fname := fieldName(pr.Fields[i])
						if !contains(selects, fname) {
							selects = append(selects, fname)
						}
						break
					}
				}
			}
		} else {
			for i := range r.fieldList {
				fname := fieldName(r.fieldList[i])
				if !contains(selects, fname) {
					selects = append(selects, fname)
				}
			}
		}
	} else if len(pr.Fields) > 0 && p.Config.FieldSelectorEnabled {
		for i := range pr.Fields {
			fname := fieldName(pr.Fields[i])
			if !contains(selects, fname) {
				selects = append(selects, fname)
			}
		}
	}

	result := dbs.
		Unscoped().
		Table("(?) AS s", query)

	if len(selects) > 0 {
		result = result.Select(selects)
	}

	if len(causes.Params) > 0 || len(causes.WhereString) > 0 {
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
	if page.TotalPages < 1 {
		page.TotalPages = 1
	}
	if page.MaxPage < 1 {
		page.MaxPage = 1
	}
	if page.Total < 1 {
		page.MaxPage = 0
		page.TotalPages = 0
	}
	page.First = causes.Offset < 1
	page.Last = page.MaxPage == page.Page

	if hasAdapter && cKey != "" {
		if cache, err := json.Marshal(page); nil == err {
			if err := adapter.Set(cKey, string(cache)); err != nil {
				log.Println(err)
			}
		}
	}

	return page
}

// New Pagination instance
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
	if netHTTP, isNetHTTP := r.(http.Request); isNetHTTP {
		parsingNetHTTPRequest(&netHTTP, &pr)
	} else {
		if netHTTPp, isNetHTTPp := r.(*http.Request); isNetHTTPp {
			parsingNetHTTPRequest(netHTTPp, &pr)
		} else {
			if fastHTTP, isFastHTTP := r.(fasthttp.Request); isFastHTTP {
				parsingFastHTTPRequest(&fastHTTP, &pr)
			} else {
				if fastHTTPp, isFastHTTPp := r.(*fasthttp.Request); isFastHTTPp {
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
		p.Filters.Fields = p.Fields
	} else if ok2 {
		iface := []interface{}{}
		if e := json.Unmarshal([]byte(s), &iface); nil == e && len(iface) > 0 {
			p.Filters = arrayToFilter(iface, p.Config)
		}
		p.Filters.Fields = p.Fields
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
		if !p.Config.CustomParamEnabled {
			var postData parameter
			if err := decoder.Decode(&postData); nil == err {
				param = &postData
			} else {
				log.Println(err.Error())
			}
		} else {
			var postData map[string]string
			if err := decoder.Decode(&postData); nil == err {
				generateParams(param, p.Config, func(key string) string {
					value, _ := postData[key]
					return value
				})
			} else {
				log.Println(err.Error())
			}
		}
	} else if strings.ToUpper(r.Method) == "GET" {
		query := r.URL.Query()
		if !p.Config.CustomParamEnabled {
			param.Size = query.Get("size")
			param.Page = query.Get("page")
			param.Sort = query.Get("sort")
			param.Order = query.Get("order")
			param.Filters = query.Get("filters")
			param.Fields = query.Get("fields")
		} else {
			generateParams(param, p.Config, func(key string) string {
				return query.Get(key)
			})
		}
	}

	parsingQueryString(param, p)
}

// parsingFastHTTPRequest func
func parsingFastHTTPRequest(r *fasthttp.Request, p *pageRequest) {
	param := &parameter{}
	if r.Header.IsPost() {
		b := r.Body()
		if !p.Config.CustomParamEnabled {
			var postData parameter
			if err := json.Unmarshal(b, &postData); nil == err {
				param = &postData
			} else {
				log.Println(err.Error())
			}
		} else {
			var postData map[string]string
			if err := json.Unmarshal(b, &postData); nil == err {
				generateParams(param, p.Config, func(key string) string {
					value, _ := postData[key]
					return value
				})
			} else {
				log.Println(err.Error())
			}
		}
	} else if r.Header.IsGet() {
		query := r.URI().QueryArgs()
		if !p.Config.CustomParamEnabled {
			param.Size = string(query.Peek("size"))
			param.Page = string(query.Peek("page"))
			param.Sort = string(query.Peek("sort"))
			param.Order = string(query.Peek("order"))
			param.Filters = string(query.Peek("filters"))
			param.Fields = string(query.Peek("fields"))
		} else {
			generateParams(param, p.Config, func(key string) string {
				return string(query.Peek(key))
			})
		}
	}

	parsingQueryString(param, p)
}

func parsingQueryString(param *parameter, p *pageRequest) {
	if i, e := strconv.Atoi(param.Size); nil == e {
		p.Size = i
	}

	if p.Size == 0 {
		if p.Config.DefaultSize > 0 {
			p.Size = int(p.Config.DefaultSize)
		} else {
			p.Size = 10
		}
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

	if param.Fields != "" {
		re := regexp.MustCompile(`[^A-z0-9_\.,]+`)
		if fields := strings.Split(param.Fields, ","); len(fields) > 0 {
			for i := range fields {
				fieldByte := re.ReplaceAll([]byte(fields[i]), []byte(""))
				if field := string(fieldByte); field != "" {
					p.Fields = append(p.Fields, field)
				}
			}
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
	param.Page = findValue(config.PageParams, "page")
	param.Size = findValue(config.SizeParams, "size")
	param.Order = findValue(config.OrderParams, "order")
	param.Filters = findValue(config.FilterParams, "filters")
	param.Fields = findValue(config.FieldsParams, "fields")
}

//gocyclo:ignore
func arrayToFilter(arr []interface{}, config Config) pageFilters {
	filters := pageFilters{
		Single: false,
	}

	operatorEscape := regexp.MustCompile(`[^A-z=\<\>\-\+\^/\*%&! ]+`)
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
					if column, ok := i.(string); ok {
						filters.Column = column
						filters.Operator = "="
						filters.Single = true
					}
				} else if k == 1 {
					filters.Value = i
					if nil == i {
						filters.Operator = "IS"
					}
				}
			} else if arrayLen == 3 {
				if k == 0 {
					if column, ok := i.(string); ok {
						filters.Column = column
						filters.Single = true
					}
				} else if k == 1 {
					if operator, ok := i.(string); ok {
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

//gocyclo:ignore
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
				if nil == f.Value {
					wheres = append(wheres, fname, f.Operator, "NULL")
				} else {
					if strValue, isStr := f.Value.(string); isStr && strings.ToLower(strValue) == "null" {
						wheres = append(wheres, fname, f.Operator, "NULL")
					} else {
						wheres = append(wheres, fname, f.Operator, "?")
						params = append(params, f.Value)
					}
				}
			case "BETWEEN":
				if values, ok := f.Value.([]interface{}); ok && len(values) >= 2 {
					wheres = append(wheres, "(", fname, f.Operator, "? AND ?", ")")
					params = append(params, valueFixer(values[0]), valueFixer(values[1]))
				}
			case "IN", "NOT IN":
				if values, ok := f.Value.([]interface{}); ok {
					wheres = append(wheres, fname, f.Operator, "?")
					params = append(params, valueFixer(values))
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
				params = append(params, valueFixer(f.Value))
			}
		}
	}

	return wheres, params
}

func valueFixer(n interface{}) interface{} {
	var values []interface{}
	if rawValues, ok := n.([]interface{}); ok {
		for i := range rawValues {
			values = append(values, valueFixer(rawValues[i]))
		}

		return values
	}
	if nil != n && reflect.TypeOf(n).Name() == "float64" {
		strValue := fmt.Sprintf("%v", n)
		if match, e := regexp.Match(`^[0-9]+$`, []byte(strValue)); nil == e && match {
			v, err := strconv.ParseInt(strValue, 10, 64)
			if nil == err {
				return v
			}
		}
	}

	return n
}

func contains(source []string, value string) bool {
	found := false
	for i := range source {
		if source[i] == value {
			found = true
			break
		}
	}

	return found
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

// Config for customize pagination result
type Config struct {
	Operator             string
	FieldWrapper         string
	ValueWrapper         string
	DefaultSize          int64
	SmartSearch          bool
	Statement            *gorm.Statement `json:"-"`
	CustomParamEnabled   bool
	SortParams           []string
	PageParams           []string
	OrderParams          []string
	SizeParams           []string
	FilterParams         []string
	FieldsParams         []string
	FieldSelectorEnabled bool
	CacheAdapter         *gocache.AdapterInterface `json:"-"`
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
	Fields      []string
}

// Page result wrapper
type Page struct {
	Items      interface{} `json:"items"`
	Page       int64       `json:"page"`
	Size       int64       `json:"size"`
	MaxPage    int64       `json:"max_page"`
	TotalPages int64       `json:"total_pages"`
	Total      int64       `json:"total"`
	Last       bool        `json:"last"`
	First      bool        `json:"first"`
	Visible    int64       `json:"visible"`
}

// parameter struct
type parameter struct {
	Page    string      `json:"page"`
	Size    string      `json:"size"`
	Sort    string      `json:"sort"`
	Order   string      `json:"order"`
	Fields  string      `json:"fields"`
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
	Fields  []string
}

// sortOrder struct
type sortOrder struct {
	Column    string
	Direction string
}

func createCacheKey(cachePrefix string, pr pageRequest) string {
	key := ""
	if bte, err := json.Marshal(pr); nil == err && cachePrefix != "" {
		key = fmt.Sprintf("%s%x", cachePrefix, md5.Sum(bte))
	}

	return key
}
