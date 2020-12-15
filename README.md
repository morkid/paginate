# Gorm Pagination

Simple way to paginate gorm result. [Gorm](https://github.com/go-gorm/gorm) Pagination is compatible for [net/http](https://golang.org/pkg/net/http/) or [fasthttp](https://github.com/valyala/fasthttp). Also support for many frameworks are based on net/http or fasthttp.

## Installation

```bash
go get -u github.com/morkid/paginate
```

## Simple usage

See example below:  
- [net/http](#nethttp-example)
- [Fasthttp](#fasthttp-example)
- [Fiber](#fiber-example)
- [Echo](#echo-example)

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
    SELECT * FROM user WHERE age BETWEEN (20 AND 25) LIMIT 10 OFFSET 0
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

## Example Usage

### Net/HTTP Example

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