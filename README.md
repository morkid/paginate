# Gorm Pagination

[![Go Reference](https://pkg.go.dev/badge/github.com/morkid/paginate.svg)](https://pkg.go.dev/github.com/morkid/paginate)
[![CircleCI](https://circleci.com/gh/morkid/paginate.svg?style=svg)](https://circleci.com/gh/morkid/paginate)
[![Github Actions](https://github.com/morkid/paginate/workflows/Go/badge.svg)](https://github.com/morkid/paginate/actions)
[![Build Status](https://travis-ci.com/morkid/paginate.svg?branch=master)](https://travis-ci.com/morkid/paginate)
[![Go Report Card](https://goreportcard.com/badge/github.com/morkid/paginate)](https://goreportcard.com/report/github.com/morkid/paginate)
[![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/morkid/paginate)](https://github.com/morkid/paginate/releases)

Simple way to paginate gorm result. [Gorm](https://github.com/go-gorm/gorm) Pagination is compatible for [net/http](https://golang.org/pkg/net/http/) or [fasthttp](https://github.com/valyala/fasthttp). This library also supports many frameworks are based on net/http or fasthttp.

## Table Of Contents
- [Installation](#installation)
- [Configuration](#configuration)
- [Paginate using http request](#paginate-using-http-request)
- [Example usage](#example-usage)
  - [net/http](#nethttp-example)
  - [Fasthttp](#fasthttp-example)
  - [Mux Router](#mux-router-example)
  - [Fiber](#fiber-example)
  - [Echo](#echo-example)
  - [Gin](#gin-example)
  - [Martini](#martini-example)
  - [Beego](#beego-example)
  - [jQuery DataTable Integration](#jquery-datatable-integration)
  - [jQuery Select2 Integration](#jquery-select2-integration)
- [Filter format](#filter-format)
- [Customize default configuration](#customize-default-configuration)
- [Override results](#override-results)
- [Limitations](#limitations)
- [License](#license)

## Installation

```bash
go get -u github.com/morkid/paginate
```

## Configuration

```go
var db *gorm.DB = ...
var req *http.Request = ...
// or
// var req *fasthttp.Request

model := db.Where("id > ?", 1).Model(&Article{})
page := paginate.New().Response(model, req, &[]Article{})

log.Println(page.Total)
log.Println(page.Items)
log.Println(page.First)
log.Println(page.Last)

```
you can customize config with `paginate.Config` struct.  
```go
pg := paginate.New(&paginate.Config{
    DefaultSize: 50,
})
```
see more about [customize default configuration](#customize-default-configuration).

## Paginate using http request
example paging, sorting and filtering:  
1. `http://localhost:3000/?size=10&page=0&sort=-name`  
    produces:
    ```sql
    SELECT * FROM user ORDER BY name DESC LIMIT 10 OFFSET 0
    ```
    `JSON` response:  
    ```js
    {
        // result items
        "items": [
            {
                "id": 1,
                "name": "john",
                "age": 20
            }
        ],
        "page": 0, // current selected page
        "size": 10, // current limit or size per page
        "max_page": 0, // maximum page
        "total_pages": 1, // total pages
        "total": 1, // total matches including next page
        "visible": 1, // total visible on current page
        "last": true, // if response is first page
        "first": true // if response is last page
    }
    ```
2. `http://localhost:3000/?size=10&page=1&sort=-name,id`  
    produces:
    ```sql
    SELECT * FROM user ORDER BY name DESC, id ASC LIMIT 10 OFFSET 10
    ```
3. `http://localhost:3000/?filters=["name","john"]`  
    produces:
    ```sql
    SELECT * FROM user WHERE name = 'john' LIMIT 10 OFFSET 0
    ```
4. `http://localhost:3000/?filters=["name","like","john"]`  
    produces:
    ```sql
    SELECT * FROM user WHERE name LIKE '%john%' LIMIT 10 OFFSET 0
    ```
5. `http://localhost:3000/?filters=["age","between",[20, 25]]`  
    produces:
     ```sql
    SELECT * FROM user WHERE ( age BETWEEN 20 AND 25 ) LIMIT 10 OFFSET 0
    ```
6. `http://localhost:3000/?filters=[["name","like","john%25"],["OR"],["age","between",[20, 25]]]`  
    produces:
     ```sql
    SELECT * FROM user WHERE (
        (name LIKE '%john\%%' ESCAPE '\') OR (age BETWEEN (20 AND 25))
    ) LIMIT 10 OFFSET 0
    ```
7. `http://localhost:3000/?filters=[[["name","like","john"],["AND"],["name","not like","doe"]],["OR"],["age","between",[20, 25]]]`  
    produces:
     ```sql
    SELECT * FROM user WHERE (
        (
            (name LIKE '%john%')
                    AND
            (name NOT LIKE '%doe%')
        ) 
        OR 
        (age BETWEEN (20 AND 25))
    ) LIMIT 10 OFFSET 0
    ```
8. `http://localhost:3000/?filters=["name","IS NOT",null]`  
    produces:
    ```sql
    SELECT * FROM user WHERE name IS NOT NULL LIMIT 10 OFFSET 0
    ```
9. Using `POST` method:  
   ```bash
   curl -X POST \
   -H 'Content-type: application/json' \
   -d '{"page":"1","size":"20","sort":"-name","filters":["name","john"]}' \
   http://localhost:3000/
   ```  

## Example usage

### NetHTTP Example

```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()

    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        model := db.Joins("User").Model(&Article{})
        paginated := pg.Response(model, r, &[]Article{})
        j, _ := json.Marshal(paginated)
        w.Header().Set("Content-type", "application/json")
        w.Write(j)
    })

    log.Fatal(http.ListenAndServe(":3000", nil))
}
```

### Fasthttp Example

```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()

    fasthttp.ListenAndServe(":3000", func(ctx *fasthttp.RequestCtx) {
        model := db.Joins("User").Model(&Article{})
        paginated := pg.Response(model, &ctx.Request, &[]Article{})
        j, _ := json.Marshal(paginated)
        ctx.SetContentType("application/json")
        ctx.SetBody(j)
    })
}
```

### Mux Router Example
```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()
    app := mux.NewRouter()
    app.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
        model := db.Joins("User").Model(&Article{})
        paginated := pg.Response(model, req, &[]Article{})
        j, _ := json.Marshal(paginated)
        w.Header().Set("Content-type", "application/json")
        w.Write(j)
    }).Methods("GET")
    http.Handle("/", app)
    http.ListenAndServe(":3000", nil)
}
```

### Fiber example

```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()
    app := fiber.New()
    app.Get("/", func(c *fiber.Ctx) error {
        model := db.Joins("User").Model(&Article{})
        return c.JSON(pg.Response(model, c.Request(), &[]Article{}))
    })

    app.Listen(":3000")
}
```

### Echo example

```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()
    app := echo.New()
    app.GET("/", func(c echo.Context) error {
        model := db.Joins("User").Model(&Article{})
        return c.JSON(200, pg.Response(model, c.Request(), &[]Article{}))
    })

    app.Logger.Fatal(app.Start(":3000"))
}
```

### Gin Example

```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()
    app := gin.Default()
    app.GET("/", func(c *gin.Context) {
        model := db.Joins("User").Model(&Article{})
        c.JSON(200, pg.Response(model, c.Request, &[]Article{}))
    })
    app.Run(":3000")
}

```

### Martini Example

```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()
    app := martini.Classic()
    app.Use(render.Renderer())
    app.Get("/", func(req *http.Request, r render.Render) {
        model := db.Joins("User").Model(&Article{})
        r.JSON(200, pg.Response(model, req, &[]Article{}))
    })
    app.Run()
}
```
### Beego Example

```go
package main

import (
    "github.com/morkid/paginate"
    ...
)

func main() {
    // var db *gorm.DB
    pg := paginate.New()
    web.Get("/", func(c *context.Context) {
        model := db.Joins("User").Model(&Article{})
        c.Output.JSON(
            pg.Response(model, c.Request, &[]Article{}), false, false)
    })
    web.Run(":3000")
}
```

### jQuery DataTable Integration

```js
var logicalOperator = "OR"

$('#myTable').DataTable({

    columns: [
        {
            title: "Author",
            data: "user.name"
        }, {
            title: "Title",
            data: "title"
        }
    ],

    processing: true,
    
    serverSide: true,

    ajax: {
        cache: true,
        url: "http://localhost:3000/articles",
        dataSrc: function(json) {
            json.recordsTotal = json.visible
            json.recordsFiltered = json.total
            return json.items
        },
        data: function(params) {
            var custom = {
                page: !params.start ? 0 : Math.round(params.start / params.length),
                size: params.length
            }

            if (params.order.length > 0) {
                var sorts = []
                for (var o in params.order) {
                    var order = params.order[o]
                    if (params.columns[order.column].orderable != false) {
                        var sort = order.dir != 'desc' ? '' : '-'
                        sort += params.columns[order.column].data
                        sorts.push(sort)
                    }
                }
                custom.sort = sorts.join()
            }

            if (params.search.value) {
                var columns = []
                for (var c in params.columns) {
                    var col = params.columns[c]
                    if (col.searchable == false) {
                        continue
                    }
                    columns.push(JSON.stringify([col.data, "like", encodeURIComponent(params.search.value.toLowerCase())]))
                }
                custom.filters = '[' + columns.join(',["' + logicalOperator + '"],') + ']'
            }

            return custom
        }
    },
})
```

### jQuery Select2 Integration

```js
$('#mySelect').select2({
    ajax: {
        url: "http://localhost:3000/users",
        processResults: function(json) {
            json.items.forEach(item => {
                item.text = item.name
            })
            // optional
            if (json.first) json.items.unshift({id: 0, text: 'All'})

            return {
                results: json.items,
                pagination: {
                    more: json.last == false
                }
            }
        },
        data: function(params) {
            var filters = [
                ["name", "like", params.term]
            ]
            var custom = {
                filters: params.term ? JSON.stringify(filters) : "",
                sort: "name",
                page: params.page && params.page - 1 ? params.page - 1 : 0
            }

            return custom
        },
    }
})
```


## Filter format

The format of filter param is a json encoded of multidimensional array.  
Maximum array members is three, first index is `column_name`, second index is `operator` and third index is `values`, you can also pass array to values.  

```js
// Format:
["column_name", "operator", "values"]

// Example:
["age", "=", 20]
// Shortcut:
["age", 20]

// Produces:
// WHERE age = 20
```

Single array member is known as **Logical Operator**.
```js
// Example
[["age", "=", 20],["or"],["age", "=", 25]]

// Produces:
// WHERE age = 20 OR age = 25
```

You are allowed to send array inside a value.  
```js
["age", "between", [20, 30] ]
// Produces:
// WHERE age BETWEEN 20 AND 30

["age", "not in", [20, 21, 22, 23, 24, 25, 26, 26] ]
// Produces:
// WHERE age NOT IN(20, 21, 22, 23, 24, 25, 26, 26)
```

You can filter nested condition with deep array.  
```js
[
    [
        ["age", ">", 20],
        ["and"]
        ["age", "<", 30]
    ],
    ["and"],
    ["name", "like", "john"],
    ["and"],
    ["name", "like", "doe"]
]
// Produces:
// WHERE ( (age > 20 AND age < 20) and name like '%john%' and name like '%doe%' )
```

For `null` value, you can send string `"null"` or `null` value, *(lower)*
```js
// Wrong request
[ "age", "is not", NULL ]
[ "age", "is not", Null ]

// Right request
[ "age", "is not", "NULL" ]
[ "age", "is not", "Null" ]
[ "age", "is not", "null" ]
[ "age", "is not", null ]
```

## Customize default configuration

You can customize the default configuration with `paginate.Config` struct. 

```go
pg := paginate.New(&paginate.Config{
    DefaultSize: 50,
})
```

Config             | Type       | Default               | Description
------------------ | ---------- | --------------------- | -------------
Operator           | `string`   | `OR`                  | Default conditional operator if no operator specified.<br>For example<br>`GET /user?filters=[["name","like","jo"],["age",">",20]]`,<br>produces<br>`SELECT * FROM user where name like '%jo' OR age > 20`
FieldWrapper       | `string`   | `LOWER(%s)`           | FieldWrapper for `LIKE` operator *(for postgres default is: `LOWER((%s)::text)`)*
DefaultSize        | `int64`    | `10`                  | Default size or limit per page
SmartSearch        | `bool`     | `false`               | Enable smart search *(Experimental feature)*
CustomParamEnabled | `bool`     | `false`               | Enable custom request parameter
SortParams         | `[]string` | `[]string{"sort"}`    | if `CustomParamEnabled` is `true`,<br>you can set the `SortParams` with custom parameter names.<br>For example: `[]string{"sorting", "ordering", "other_alternative_param"}`.<br>The following requests will capture same result<br>`?sorting=-name`<br>or `?ordering=-name`<br>or `?other_alternative_param=-name`<br>or `?sort=-name`
PageParams         | `[]string` | `[]string{"page"}`    | if `CustomParamEnabled` is `true`,<br>you can set the `PageParams` with custom parameter names.<br>For example:<br>`[]string{"number", "num", "other_alternative_param"}`.<br>The following requests will capture same result `?number=0`<br>or `?num=0`<br>or `?other_alternative_param=0`<br>or `?page=0`
SizeParams         | `[]string` | `[]string{"size"}`    | if `CustomParamEnabled` is `true`,<br>you can set the `SizeParams` with custom parameter names.<br>For example:<br>`[]string{"limit", "max", "other_alternative_param"}`.<br>The following requests will capture same result `?limit=50`<br>or `?limit=50`<br>or `?other_alternative_param=50`<br>or `?max=50`
FilterParams       | `[]string` | `[]string{"filters"}` | if `CustomParamEnabled` is `true`,<br>you can set the `FilterParams` with custom parameter names.<br>For example:<br>`[]string{"search", "find", "other_alternative_param"}`.<br>The following requests will capture same result<br>`?search=["name","john"]`<br>or `?find=["name","john"]`<br>or `?other_alternative_param=["name","john"]`<br>or `?filters=["name","john"]`

## Override results

You can override result with custom function.  

```go
// var db = *gorm.DB
// var httpRequest ... net/http or fasthttp instance
// Example override function
override := func(article *Article) {
    if article.UserID > 0 {
        article.Title = fmt.Sprintf(
            "%s written by %s", article.Title, article.User.Name)
    }
}

var articles []Article
model := db.Joins("User").Model(&Article{})

pg := paginate.New()
result := pg.Response(model, httpRequest, &articles)
for index := range articles {
    override(&articles[index])
}

log.Println(result.Items)

```

## Limitations

Gorm pagination doesn't support for customized json or table field name.  
Make sure your struct properties have same name with gorm column and json property before you expose them.  

Example bad configuration:  

```go

type User struct {
    gorm.Model
    UserName       string `gorm:"column:nickname" json:"name"`
    UserAddress    string `gorm:"column:user_address" json:"address"`
}

// request: GET /path/to/endpoint?sort=-name,address
// response: "items": [] with sql error (column name not found)
```

Best practice:
```go
type User struct {
    gorm.Model
    Name       string `gorm:"column:name" json:"name"`
    Address    string `gorm:"column:address" json:"address"`
}

```

## License

Published under the [MIT License](https://github.com/morkid/paginate/blob/master/LICENSE).