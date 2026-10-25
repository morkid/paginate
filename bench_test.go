package paginate

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// BenchmarkParseRequest measures request parsing (filters, sorts, bounds).
func BenchmarkParseRequest(b *testing.B) {
	req := &Request{
		Page:    2,
		Size:    20,
		Sort:    "user.name,-id",
		Filters: `[["name","like","john"],["OR"],["age",">",18]]`,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		parseRequest(req, Config{})
	}
}

// BenchmarkResponseCommon measures a realistic filtered+sorted page query.
func BenchmarkResponseCommon(b *testing.B) {
	dsn := "file:bench?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		b.Fatal(err)
	}
	type benchUser struct {
		ID     uint `gorm:"primaryKey"`
		Name   string
		Tenant string
	}
	if err := db.AutoMigrate(&benchUser{}); err != nil {
		b.Fatal(err)
	}
	var n int64
	db.Model(&benchUser{}).Count(&n)
	if n == 0 {
		for i := 0; i < 200; i++ {
			db.Create(&benchUser{
				Name:   fmt.Sprintf("user %d", i),
				Tenant: fmt.Sprintf("t%d", i%3),
			})
		}
	}

	req := &http.Request{
		Method: "GET",
		URL: &url.URL{
			RawQuery: "page=2&size=20&sort=-name&filters=" +
				url.QueryEscape(`[["tenant","=","t1"]]`),
		},
	}
	pg := New()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out []benchUser
		page := pg.With(db.Model(&benchUser{})).Request(req).Response(&out)
		if page.Total == 0 {
			b.Fatal("expected rows")
		}
	}
}
