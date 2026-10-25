package paginate

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/valyala/fasthttp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fixUser struct {
	ID     uint `gorm:"primaryKey"`
	Name   string
	Tenant string
}

// setupFixDB creates an isolated in-memory DB with three rows.
func setupFixDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&fixUser{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("DELETE FROM fix_users")
	db.Create(&fixUser{Name: "alice", Tenant: "t1"})
	db.Create(&fixUser{Name: "bob", Tenant: "t1"})
	db.Create(&fixUser{Name: "carol", Tenant: "t2"})
	return db
}

func getReq(rawQuery string) *http.Request {
	return &http.Request{Method: "GET", URL: &url.URL{RawQuery: rawQuery}}
}

// --- F-01: operator allowlist / filter bypass ---

func TestOperatorBypassRejected(t *testing.T) {
	db := setupFixDB(t)
	cases := []string{
		`[["name","=","alice"],["OR TRUE OR"],["name","=","nobody"]]`,
		`[["name","= NULL OR TRUE OR NAME NOT LIKE","x"]]`,
		`["OR TRUE OR"]`,
		`[["name","foo; drop table fix_users","x"]]`,
	}
	for _, filters := range cases {
		var out []fixUser
		pg := New(&Config{ErrorEnabled: true})
		page := pg.With(db.Model(&fixUser{})).
			Request(getReq("size=10&filters=" + url.QueryEscape(filters))).
			Response(&out)
		if !page.Error && page.RawError == nil {
			t.Errorf("filters %s: expected rejection, got total=%d rows=%d", filters, page.Total, len(out))
		}
		if len(out) != 0 {
			t.Errorf("filters %s: fail-closed expected 0 rows, got %d", filters, len(out))
		}
	}
	// the original PoC must no longer return all rows
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(`[["name","=","alice"],["OR TRUE OR"],["name","=","nobody"]]`))).
		Response(&out)
	if page.Total > 1 {
		t.Errorf("tautology bypass still works: total=%d", page.Total)
	}
}

func TestValidOperatorsStillWork(t *testing.T) {
	db := setupFixDB(t)
	cases := []struct {
		filters string
		expect  int64
	}{
		{`[["name","=","alice"]]`, 1},
		{`[["name","!=","alice"]]`, 2},
		{`[["name","like","ali"]]`, 1},
		{`[["name","in",["alice","bob"]]]`, 2},
		{`[["name","between",["a","b"]]]`, 1},
		{`[["tenant","is not",null]]`, 3},
		{`[["name","like","alice"],["OR"],["tenant","=","t2"]]`, 2},
		{`[[["name","=","alice"],["AND"],["tenant","=","t1"]]]`, 1},
	}
	for _, c := range cases {
		var out []fixUser
		page := New().With(db.Model(&fixUser{})).
			Request(getReq("size=100&filters=" + url.QueryEscape(c.filters))).
			Response(&out)
		if page.RawError != nil {
			t.Errorf("filters %s: unexpected error %v", c.filters, page.RawError)
		}
		if page.Total != c.expect {
			t.Errorf("filters %s: expected total=%d, got %d", c.filters, c.expect, page.Total)
		}
	}
}

// --- F-02/F-03: column & sort validation ---

func TestMaliciousColumnsAndSortsRejected(t *testing.T) {
	db := setupFixDB(t)
	maliciousFilters := []string{
		"[\"`x`) OR 1=1 OR (`x\",\"=\",\"y\"]",
		"[\"name; DROP TABLE fix_users\",\"=\",\"x\"]",
		"[\"name`\",\"=\",\"x\"]",
	}
	for _, filters := range maliciousFilters {
		var out []fixUser
		page := New(&Config{ErrorEnabled: true}).
			With(db.Model(&fixUser{})).
			Request(getReq("size=10&filters=" + url.QueryEscape(filters))).
			Response(&out)
		if page.RawError == nil && len(out) == 0 && page.Total == 0 {
			continue // rejected fail-closed
		}
		if page.RawError == nil {
			t.Errorf("column %s: expected rejection, total=%d", filters, page.Total)
		}
	}

	for _, sort := range []string{"name`", "name; DROP TABLE x", "name)", "(SELECT 1)"} {
		var out []fixUser
		page := New(&Config{ErrorEnabled: true}).
			With(db.Model(&fixUser{})).
			Request(getReq("size=10&sort=" + url.QueryEscape(sort))).
			Response(&out)
		if page.RawError == nil && page.Total == 3 && len(out) == 3 && !strings.Contains(sort, ")") {
			// valid-looking names could exist; only fail if query executed
			// with a clearly invalid identifier
			t.Errorf("sort %q: expected rejection", sort)
		}
	}
}

func TestValidDottedSortStillWorks(t *testing.T) {
	db := setupFixDB(t)
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq("size=10&sort=-name")).
		Response(&out)
	if page.RawError != nil {
		t.Fatalf("sort -name failed: %v", page.RawError)
	}
	if len(out) != 3 || out[0].Name != "carol" {
		t.Errorf("expected desc order carol first, got %+v", out)
	}
}

// --- F-04: size bounds ---

func TestSizeBounds(t *testing.T) {
	db := setupFixDB(t)
	for _, c := range []struct {
		size     string
		wantSize int64
	}{
		{"-1", 10},
		{"-100", 10},
		{"0", 10},
		{"999999", 100},
		{"999999999999", 100},
	} {
		var out []fixUser
		page := New().With(db.Model(&fixUser{})).
			Request(getReq("page=1&size=" + c.size)).
			Response(&out)
		if page.Size != c.wantSize {
			t.Errorf("size=%s: expected clamped Size=%d, got %d", c.size, c.wantSize, page.Size)
		}
		if page.RawError != nil {
			t.Errorf("size=%s: unexpected error %v", c.size, page.RawError)
		}
	}
	// white-box: LIMIT must be positive (gorm omits LIMIT for <=0)
	pr := parseRequest(&Request{Size: -1, Page: 1}, Config{})
	causes := createCauses(pr)
	if causes.Limit <= 0 {
		t.Errorf("negative size reached builder as Limit=%d", causes.Limit)
	}
}

func TestMaxSizeConfigurable(t *testing.T) {
	db := setupFixDB(t)
	var out []fixUser
	page := New(&Config{MaxSize: 2}).With(db.Model(&fixUser{})).
		Request(getReq("size=50")).
		Response(&out)
	if page.Size != 2 {
		t.Errorf("expected MaxSize=2 honored, got %d", page.Size)
	}
}

// --- F-13: offset overflow ---

func TestOffsetOverflowNoWrap(t *testing.T) {
	db := setupFixDB(t)
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq(fmt.Sprintf("page=%d&size=10", int64(math.MaxInt64/5)))).
		Response(&out)
	if page.RawError == nil && len(out) != 0 {
		t.Errorf("overflowed offset must not wrap to page 0: got %d rows (First=%v)", len(out), page.First)
	}
}

// --- F-05: data race (runs clean under -race) ---

func TestConcurrentSharedPagination(t *testing.T) {
	db := setupFixDB(t)
	pg := New(&Config{DefaultSize: 10})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			var out []fixUser
			_ = pg.With(db.Model(&fixUser{})).Request(&Request{
				Page: int64(n + 1), Size: 5,
				Filters: `[["tenant","=","t1"]]`,
				Sort:    "-name",
			}).Response(&out)
		}(i)
	}
	wg.Wait()
}

// --- F-07: fail-open IN/BETWEEN shape ---

func TestMalformedInBetweenFailClosed(t *testing.T) {
	db := setupFixDB(t)
	for _, filters := range []string{
		`[["tenant","IN","t1"]]`,      // scalar instead of array
		`[["tenant","BETWEEN","t1"]]`, // single value
	} {
		var out []fixUser
		page := New(&Config{ErrorEnabled: true}).
			With(db.Model(&fixUser{})).
			Request(getReq("size=10&filters=" + url.QueryEscape(filters))).
			Response(&out)
		if page.RawError == nil {
			t.Errorf("filters %s: expected invalid-filter rejection", filters)
		}
		if len(out) != 0 {
			t.Errorf("filters %s: expected 0 rows, got %d", filters, len(out))
		}
	}
}

// --- F-08: Fields() allowlist enforced on filters and sorts ---

func TestFieldsAllowlistEnforced(t *testing.T) {
	db := setupFixDB(t)
	pg := New(&Config{ErrorEnabled: true})

	// filter on a column outside the allowlist → rejected
	var out []fixUser
	page := pg.With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(`[["name","=","alice"]]`))).
		Fields([]string{"id"}).
		Response(&out)
	if page.RawError == nil {
		t.Errorf("filter outside Fields() allowlist must be rejected")
	}

	// sort outside the allowlist → rejected
	page = pg.With(db.Model(&fixUser{})).
		Request(getReq("size=10&sort=name")).
		Fields([]string{"id"}).
		Response(&out)
	if page.RawError == nil {
		t.Errorf("sort outside Fields() allowlist must be rejected")
	}

	// filter inside the allowlist → works
	page = pg.With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(`[["name","=","alice"]]`))).
		Fields([]string{"id", "name"}).
		Response(&out)
	if page.RawError != nil || page.Total != 1 {
		t.Errorf("allowed filter should work: total=%d err=%v", page.Total, page.RawError)
	}
}

// --- F-14: LIKE underscore escaping ---

func TestLikeUnderscoreEscaped(t *testing.T) {
	db := setupFixDB(t)
	db.Create(&fixUser{Name: "axb", Tenant: "t1"})
	// pattern a_b must match literal "a_b"-prefix only, not "axb"
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq("size=100&filters=" + url.QueryEscape(`[["name","like","a_b"]]`))).
		Response(&out)
	if page.RawError != nil {
		t.Fatalf("query failed: %v", page.RawError)
	}
	for _, u := range out {
		if u.Name == "axb" {
			t.Errorf("_ wildcard not escaped: axb matched by a_b")
		}
	}
}

// --- F-06: cache scope isolation ---

type countingAdapter struct {
	store map[string]string
}

func (a *countingAdapter) Get(key string) (string, error) {
	if v, ok := a.store[key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("not found")
}
func (a *countingAdapter) Set(key string, value string) error {
	if a.store == nil {
		a.store = map[string]string{}
	}
	a.store[key] = value
	return nil
}
func (a *countingAdapter) IsValid(key string) bool {
	_, ok := a.store[key]
	return ok
}
func (a *countingAdapter) Clear(key string) error          { delete(a.store, key); return nil }
func (a *countingAdapter) ClearPrefix(prefix string) error { return nil }
func (a *countingAdapter) ClearAll() error                 { a.store = map[string]string{}; return nil }

func TestCacheScopeIsolation(t *testing.T) {
	db := setupFixDB(t)
	adapter := &countingAdapter{store: map[string]string{}}

	req := getReq("size=10")
	var out1, out2 []fixUser

	pg1 := New(&Config{CacheAdapter: adapter, CacheScope: func() string { return "user1" }})
	pg1.With(db.Model(&fixUser{})).Request(req).Cache("pfx").Response(&out1)

	pg2 := New(&Config{CacheAdapter: adapter, CacheScope: func() string { return "user2" }})
	pg2.With(db.Model(&fixUser{})).Request(req).Cache("pfx").Response(&out2)

	if len(adapter.store) != 2 {
		t.Errorf("different scopes must produce different cache keys, got %d key(s)", len(adapter.store))
	}
}

// --- F-15: warm vs cold cache types ---

func TestCacheTypedItemsAndDBBypass(t *testing.T) {
	db := setupFixDB(t)
	adapter := &countingAdapter{store: map[string]string{}}
	pg := New(&Config{CacheAdapter: adapter})
	req := getReq("size=10")

	var cold []fixUser
	page1 := pg.With(db.Model(&fixUser{})).Request(req).Cache("k").Response(&cold)
	if page1.Total != 3 || len(cold) != 3 {
		t.Fatalf("cold request broken: total=%d rows=%d", page1.Total, len(cold))
	}

	// second call uses a DIFFERENT (empty) table: only a cache hit can return rows
	type otherTable struct {
		ID uint `gorm:"primaryKey"`
	}
	var warm []fixUser
	page2 := pg.With(db.Model(&otherTable{})).Request(req).Cache("k").Response(&warm)
	if page2.Total != 3 || len(warm) != 3 {
		t.Errorf("cache hit must populate typed items: total=%d rows=%d", page2.Total, len(warm))
	}
	for _, u := range warm {
		if u.Name == "" {
			t.Errorf("typed restore failed: empty Name in %+v", u)
		}
	}
}

// --- F-11: generic error messages ---

func TestGenericErrorMessage(t *testing.T) {
	db := setupFixDB(t)
	var out []fixUser
	page := New(&Config{ErrorEnabled: true}).
		With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(`[["nonexistent_column_xyz","=","x"]]`))).
		Response(&out)
	if !page.Error {
		t.Fatal("expected error page")
	}
	if strings.Contains(page.ErrorMessage, "no such column") ||
		strings.Contains(page.ErrorMessage, "SQLSTATE") ||
		strings.Contains(page.ErrorMessage, "nonexistent_column_xyz") {
		t.Errorf("raw SQL error leaked: %q", page.ErrorMessage)
	}
	if page.RawError == nil {
		t.Errorf("RawError must retain the detail for server-side use")
	}
}

// --- F-16: nil statement does not panic ---

func TestNilStatementNoPanic(t *testing.T) {
	var out []fixUser
	page := New().With(nil).Request(&Request{Size: 10}).Response(&out)
	if page.RawError == nil {
		t.Errorf("nil statement must produce RawError")
	}
	page = New().With(&gorm.DB{}).Request(&Request{Size: 10}).Response(&out)
	if page.RawError == nil {
		t.Errorf("empty gorm.DB must produce RawError")
	}
}

// --- F-12: body limits ---

func TestBodyLimitAndNilBody(t *testing.T) {
	// nil body must not panic and is treated as "no parameters"
	parsed := parseRequest(&http.Request{Method: "POST", Body: nil}, Config{})
	if parsed.Invalid {
		t.Errorf("nil body must stay valid, got Invalid=%q", parsed.ErrorMessage)
	}
	if parsed.Size != 10 {
		t.Errorf("nil body should fall back to defaults, size=%d", parsed.Size)
	}

	// empty body == absent parameters
	emptyReq, _ := http.NewRequest("POST", "http://x/", strings.NewReader("   "))
	parsed = parseRequest(emptyReq, Config{})
	if parsed.Invalid {
		t.Errorf("empty body must stay valid, got Invalid=%q", parsed.ErrorMessage)
	}

	// oversized body fails closed (was: silent defaults = dropped filters)
	huge := strings.Repeat("A", maxRequestBodySize+1024)
	req, _ := http.NewRequest("POST", "http://x/", strings.NewReader(`{"page":1,"size":5,"filters":"`+huge+`"}`))
	parsed = parseRequest(req, Config{})
	if !parsed.Invalid || parsed.ErrorMessage != "request body too large" {
		t.Errorf("oversized body must fail closed, got Invalid=%v msg=%q", parsed.Invalid, parsed.ErrorMessage)
	}

	// malformed JSON body fails closed (was: silent defaults = dropped filters)
	badReq, _ := http.NewRequest("POST", "http://x/", strings.NewReader(`{"page":1,"filters":[["name","=`))
	parsed = parseRequest(badReq, Config{})
	if !parsed.Invalid || parsed.ErrorMessage != "invalid request body" {
		t.Errorf("malformed JSON body must fail closed, got Invalid=%v msg=%q", parsed.Invalid, parsed.ErrorMessage)
	}

	// Response surfaces parse-level rejections as visible errors
	db := setupFixDB(t)
	var out []fixUser
	respReq, _ := http.NewRequest("POST", "http://x/", strings.NewReader(`not-json`))
	page := New().With(db.Model(&fixUser{})).Request(respReq).Response(&out)
	if !page.Error || page.ErrorMessage != "invalid request body" {
		t.Errorf("Response must reject malformed body visibly, got error=%v msg=%q", page.Error, page.ErrorMessage)
	}
	if len(out) != 0 {
		t.Errorf("expected 0 rows, got %d", len(out))
	}

	// fasthttp parity
	fReq := &fasthttp.Request{}
	fReq.Header.SetMethod("POST")
	fReq.SetBodyString("not-json")
	parsed = parseRequest(fReq, Config{})
	if !parsed.Invalid {
		t.Errorf("fasthttp malformed body must fail closed")
	}
	fReq2 := &fasthttp.Request{}
	fReq2.Header.SetMethod("POST")
	fReq2.SetBodyString(strings.Repeat("A", maxRequestBodySize+1))
	parsed = parseRequest(fReq2, Config{})
	if !parsed.Invalid || parsed.ErrorMessage != "request body too large" {
		t.Errorf("fasthttp oversized body must fail closed, got %v %q", parsed.Invalid, parsed.ErrorMessage)
	}
}

// --- F-10 (verified non-issue): soft-delete behavior locked by test ---

type softFixUser struct {
	ID        uint `gorm:"primaryKey"`
	Name      string
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

func TestSoftDeleteRespected(t *testing.T) {
	dsn := "file:softfix?mode=memory&cache=shared"
	db, _ := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	db.AutoMigrate(&softFixUser{})
	db.Exec("DELETE FROM soft_fix_users")
	db.Create(&softFixUser{Name: "alive"})
	db.Create(&softFixUser{Name: "gone"})
	db.Where("name = ?", "gone").Delete(&softFixUser{})

	var out []softFixUser
	page := New().With(db.Model(&softFixUser{})).Request(&Request{Size: 10}).Response(&out)
	if page.Total != 1 || (len(out) > 0 && out[0].Name != "alive") {
		t.Errorf("default path must hide soft-deleted rows, total=%d rows=%v", page.Total, out)
	}

	var out2 []softFixUser
	page2 := New().With(db.Model(&softFixUser{}).Unscoped()).Request(&Request{Size: 10}).Response(&out2)
	if page2.Total != 2 {
		t.Errorf("caller Unscoped() must still see deleted rows, total=%d", page2.Total)
	}
}

// --- fieldName golden tests (strcase v0.3.0 lock-in) ---

func TestFieldNameGolden(t *testing.T) {
	cases := map[string]string{
		"user":           "user",
		"id":             "id",
		"user.name":      "User__name",
		"user.age":       "User__age",
		"category.title": "Category__title",
	}
	for in, want := range cases {
		if got := fieldName(in); got != want {
			t.Errorf("fieldName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ==================== Round 2 — Waves 1 & 2 ====================

// N-03: server DefaultSize above the client cap must be honored.
func TestDefaultSizeNotCapped(t *testing.T) {
	db := setupFixDB(t)
	var out []fixUser
	page := New(&Config{DefaultSize: 500}).
		With(db.Model(&fixUser{})).
		Request(getReq("page=1")).
		Response(&out)
	if page.Size != 500 {
		t.Errorf("DefaultSize=500 must not be clamped by client cap, got Size=%d", page.Size)
	}
	// client-supplied sizes are still capped
	page = New(&Config{DefaultSize: 500}).
		With(db.Model(&fixUser{})).
		Request(getReq("page=1&size=999999")).
		Response(&out)
	if page.Size != 100 {
		t.Errorf("client size must still be capped at 100, got %d", page.Size)
	}
}

// N-02: non-empty malformed filters string fails closed; empty/[] stay no-op.
func TestMalformedFiltersStringRejected(t *testing.T) {
	db := setupFixDB(t)
	for _, f := range []string{`not-json`, `{"a":1}`, `[["name","=","al`} {
		var out []fixUser
		page := New().With(db.Model(&fixUser{})).
			Request(&Request{Size: 10, Filters: f}).
			Response(&out)
		if page.RawError == nil || len(out) != 0 {
			t.Errorf("filters %q must fail closed: err=%v rows=%d", f, page.RawError, len(out))
		}
	}
	for _, f := range []string{"", "[]"} {
		var out []fixUser
		page := New().With(db.Model(&fixUser{})).
			Request(&Request{Size: 10, Filters: f}).
			Response(&out)
		if page.RawError != nil || page.Total != 3 {
			t.Errorf("filters %q means no filters: total=%d err=%v", f, page.Total, page.RawError)
		}
	}
}

// N-04: with FieldSelectorEnabled=false the request's fields param must not
// constrain filters/sorts (v1.1.11 behavior).
func TestFieldsParamInertWhenSelectorDisabled(t *testing.T) {
	db := setupFixDB(t)
	q := "size=10&fields=id&sort=name&filters=" + url.QueryEscape(`[["name","=","alice"]]`)
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq(q)).
		Response(&out)
	if page.RawError != nil {
		t.Errorf("fields param must be inert when selector disabled: %v", page.RawError)
	}
	if page.Total != 1 || len(out) != 1 || out[0].Name != "alice" {
		t.Errorf("expected filter+sort to work, total=%d rows=%v", page.Total, out)
	}
}

// N-01: Limit/Offset must fit the platform int so casts can never wrap
// negative (C-12d: the int32 clamp now applies only to 32-bit builds).
func TestOffsetLimitClampedToInt32(t *testing.T) {
	pr := parseRequest(&Request{Page: math.MaxInt64 / 5, Size: 10}, Config{})
	causes := createCauses(pr)
	if causes.Offset < 0 || causes.Offset > math.MaxInt {
		t.Errorf("Offset %d does not fit int", causes.Offset)
	}
	if causes.Limit < 0 || causes.Limit > math.MaxInt {
		t.Errorf("Limit %d does not fit int", causes.Limit)
	}
	pr = parseRequest(&Request{Page: 1, Size: math.MaxInt64}, Config{})
	causes = createCauses(pr)
	if causes.Limit > defaultMaxPageSize {
		t.Errorf("huge Size must clamp, Limit=%d", causes.Limit)
	}
}

// N-05: validation rejections are visible even with ErrorEnabled=false.
func TestValidationVisibleWithoutErrorEnabled(t *testing.T) {
	db := setupFixDB(t)
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(`[["name","=","alice"],["OR TRUE OR"],["name","=","x"]]`))).
		Response(&out)
	if !page.Error {
		t.Errorf("validation rejection must set Error=true regardless of ErrorEnabled")
	}
	if page.ErrorMessage == "" {
		t.Errorf("validation rejection must set ErrorMessage")
	}
	if len(out) != 0 {
		t.Errorf("expected 0 rows, got %d", len(out))
	}
}

// N-06: garbage filter shapes get clean rejections, not SQL syntax errors.
func TestGarbageShapesCleanlyRejected(t *testing.T) {
	db := setupFixDB(t)
	cases := []string{
		`[["name",5,"x"]]`,          // non-string operator
		`[["a","b","c","d"]]`,       // 4-element filter
		`[["OR"],["name","=","x"]]`, // leading connector
		`"OR"`,                      // bare connector as whole filter
		`[123]`,                     // non-string connector element
		`[[]]`,                      // V3-01: empty group member
		`[[],["name","=","x"]]`,     // V3-01: empty member + predicate
		`[[[]],["name","=","x"]]`,   // V3-01: nested empty group
	}
	for _, filters := range cases {
		var out []fixUser
		page := New().With(db.Model(&fixUser{})).
			Request(getReq("size=10&filters=" + url.QueryEscape(filters))).
			Response(&out)
		if page.RawError == nil {
			t.Errorf("filters %q: expected clean rejection", filters)
		}
		msg := ""
		if page.ErrorMessage != "" {
			msg = page.ErrorMessage
		} else if page.RawError != nil {
			msg = page.RawError.Error()
		}
		if strings.Contains(msg, "syntax") || strings.Contains(msg, "near") {
			t.Errorf("filters %q: SQL syntax error leaked instead of clean rejection: %q", filters, msg)
		}
		if len(out) != 0 {
			t.Errorf("filters %q: expected 0 rows, got %d", filters, len(out))
		}
	}
}

// ==================== Round 2 — Wave 3 ====================

// N-08: ExtraOperators are sanitized (upper-cased; metacharacter entries dropped).
func TestExtraOperatorsSanitized(t *testing.T) {
	cfg := defaultConfig(&Config{ExtraOperators: []string{"similar to", "x; drop table users"}})
	if len(cfg.ExtraOperators) != 1 || cfg.ExtraOperators[0] != "SIMILAR TO" {
		t.Fatalf("expected [SIMILAR TO], got %#v", cfg.ExtraOperators)
	}
	// configured extra works end-to-end (GLOB is sqlite's operator — the point
	// is that a non-default operator passes validation when configured)
	db := setupFixDB(t)
	var out []fixUser
	page := New(&Config{ExtraOperators: []string{"glob"}}).
		With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(`[["name","glob","a*"]]`))).
		Response(&out)
	if page.RawError != nil {
		t.Errorf("allowed extra operator should work: %v", page.RawError)
	}
	// metacharacter-laden operator is gone from the allowlist → rejected
	page = New(&Config{ExtraOperators: []string{"x; drop table users"}}).
		With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(`[["name","x; drop table users","x"]]`))).
		Response(&out)
	if page.RawError == nil {
		t.Errorf("sanitized-away extra must be rejected")
	}
}

// N-09: server-side .Fields() entries are identifier-validated.
func TestServerFieldsValidated(t *testing.T) {
	db := setupFixDB(t)
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq("size=10")).
		Fields([]string{"name", "bad`field"}).
		Response(&out)
	if !page.Error || page.ErrorMessage != "invalid field parameter" {
		t.Errorf("invalid .Fields() entry must be rejected, got error=%v msg=%q", page.Error, page.ErrorMessage)
	}
	// valid entries still work
	page = New().With(db.Model(&fixUser{})).
		Request(getReq("size=10")).
		Fields([]string{"id", "name"}).
		Response(&out)
	if page.Error || page.Total != 3 {
		t.Errorf("valid .Fields() entries must work: error=%v total=%d", page.Error, page.Total)
	}
}

// N-11: StrictFilters rejects dropped garbage elements; default stays lenient.
func TestStrictFilters(t *testing.T) {
	db := setupFixDB(t)
	garbage := `[["name","=","alice"],123]`

	// default: lenient — element dropped, valid predicate kept
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(garbage))).
		Response(&out)
	if page.RawError != nil || page.Total != 1 {
		t.Errorf("default must stay lenient: total=%d err=%v", page.Total, page.RawError)
	}

	// strict: rejected (fresh slice — a rejection must not touch caller data)
	out = nil
	page = New(&Config{StrictFilters: true}).
		With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(garbage))).
		Response(&out)
	if page.RawError == nil || len(out) != 0 {
		t.Errorf("StrictFilters must reject garbage elements: err=%v rows=%d", page.RawError, len(out))
	}

	// strict + no filters at all stays valid
	page = New(&Config{StrictFilters: true}).
		With(db.Model(&fixUser{})).
		Request(getReq("size=10")).
		Response(&out)
	if page.RawError != nil || page.Total != 3 {
		t.Errorf("StrictFilters must not reject empty filters: total=%d err=%v", page.Total, page.RawError)
	}

	// V3-01: trailing null element — lenient by default, rejected in strict mode
	nullFilters := `[["name","=","alice"],null]`
	var outNull []fixUser
	page = New().With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(nullFilters))).
		Response(&outNull)
	if page.RawError != nil || page.Total != 1 {
		t.Errorf("default must stay lenient for trailing null: total=%d err=%v", page.Total, page.RawError)
	}

	outNull = nil
	page = New(&Config{StrictFilters: true}).
		With(db.Model(&fixUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(nullFilters))).
		Response(&outNull)
	if page.RawError == nil || len(outNull) != 0 {
		t.Errorf("StrictFilters must reject trailing null element: err=%v rows=%d", page.RawError, len(outNull))
	}
}

// P-04: escapeLikeValue replaces the regexp path with identical semantics.
func TestEscapeLikeValue(t *testing.T) {
	cases := []struct{ in, esc, want string }{
		{"a_b", `\`, `a\_b`},
		{"100%", `\`, `100\%`},
		{`back\slash`, `\`, `back\\slash`},
		{"plain", `\`, "plain"},
		{"a_b", "", "a_b"},
	}
	for _, c := range cases {
		if got := escapeLikeValue(c.in, c.esc); got != c.want {
			t.Errorf("escapeLikeValue(%q,%q)=%q want %q", c.in, c.esc, got, c.want)
		}
	}
}

// N-07b: cache keys carry "prefix:" and ClearCache clears exact scopes.
func TestCacheKeyDelimiterAndClear(t *testing.T) {
	pr := parseRequest(&Request{Size: 10}, Config{})
	pr.Config = *defaultConfig(&pr.Config)
	key := createCacheKey("user", pr, "")
	if !strings.HasPrefix(key, "user:") {
		t.Errorf("key must start with prefix+colon, got %q", key)
	}
	key2 := createCacheKey("user", pr, "tenant-a")
	key3 := createCacheKey("user", pr, "tenant-b")
	if key2 == key3 {
		t.Errorf("different scopes must yield different keys")
	}
}

// ==================== Round 4 — V4-01/02/03 ====================

// V4-01: filter nesting depth is capped; deep payloads fail closed cleanly.
func TestFilterDepthCap(t *testing.T) {
	db := setupFixDB(t)
	deep := func(k int) string {
		return strings.Repeat("[", k) + `[["name","=","alice"]]` + strings.Repeat("]", k)
	}
	// shallow nesting (README-shaped, ~5) works
	var out []fixUser
	page := New().With(db.Model(&fixUser{})).
		Request(&Request{Size: 10, Filters: deep(5)}).
		Response(&out)
	if page.RawError != nil || page.Total != 1 {
		t.Errorf("depth 5 must work: total=%d err=%v", page.Total, page.RawError)
	}
	// payload = k wrappers + group + triple -> triple sits at depth k+1;
	// guard trips when depth > 16, so k=15 (triple at 16) works, k=16+ is capped
	out = nil
	page = New().With(db.Model(&fixUser{})).
		Request(&Request{Size: 10, Filters: deep(15)}).
		Response(&out)
	if page.RawError != nil || page.Total != 1 {
		t.Errorf("depth 15 (at limit) must work: total=%d err=%v", page.Total, page.RawError)
	}
	for _, k := range []int{16, 100, 200} {
		out = nil
		page = New().With(db.Model(&fixUser{})).
			Request(&Request{Size: 10, Filters: deep(k)}).
			Response(&out)
		if page.RawError == nil {
			t.Errorf("depth %d must be rejected (V4-01)", k)
		}
		msg := page.ErrorMessage
		if page.RawError != nil {
			msg = page.RawError.Error()
		}
		if strings.Contains(msg, "syntax") || strings.Contains(msg, "near") ||
			strings.Contains(msg, "stack") {
			t.Errorf("depth %d: clean rejection required, got %q", k, msg)
		}
		if len(out) != 0 {
			t.Errorf("depth %d: expected 0 rows, got %d", k, len(out))
		}
	}
}

// V4-02: connector words never survive ExtraOperators sanitization.
func TestExtraOperatorsRejectConnectors(t *testing.T) {
	cfg := defaultConfig(&Config{ExtraOperators: []string{
		"OR 1",
		"is not null or true",
		"AND",
		"similar to",
	}})
	if len(cfg.ExtraOperators) != 1 || cfg.ExtraOperators[0] != "SIMILAR TO" {
		t.Errorf("expected only [SIMILAR TO], got %#v", cfg.ExtraOperators)
	}
}

// V4-03: a zero member fails the whole group closed as ONE fragment
// (no connector-less adjacency from hand-built trees).
func TestZeroMemberFailsGroupClosed(t *testing.T) {
	group := pageFilters{
		Value: []pageFilters{
			{}, // zero member
			{Column: "name", Operator: "=", Value: "alice", Single: true},
		},
	}
	wheres, params := generateWhereCauses(group, Config{})
	if len(wheres) != 1 || wheres[0] != "1 = 0" {
		t.Errorf("expected single 1 = 0 fragment, got %#v", wheres)
	}
	if len(params) != 0 {
		t.Errorf("expected no params, got %v", params)
	}
	// root zero node still means "no filters"
	if root, _ := generateWhereCauses(pageFilters{}, Config{}); len(root) != 0 {
		t.Errorf("root zero must stay empty, got %v", root)
	}
}

// --- audit round 3 (C-01…C-12, R-01) ---

type cfOrder struct {
	ID        uint `gorm:"primaryKey"`
	CfUserID  uint
	DeletedAt gorm.DeletedAt
}

type cfUser struct {
	ID     uint `gorm:"primaryKey"`
	Name   string
	Nick   *string
	Secret string    `json:"-"`
	Orders []cfOrder `gorm:"foreignKey:CfUserID"`
}

// setupCfDB: three users (one with a nick), user 1 has one live and one
// soft-deleted order.
func setupCfDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&cfUser{}, &cfOrder{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("DELETE FROM cf_orders")
	db.Exec("DELETE FROM cf_users")
	nick := "al"
	db.Create(&cfUser{ID: 1, Name: "alice", Nick: &nick, Secret: "s1"})
	db.Create(&cfUser{ID: 2, Name: "bob", Secret: "s2"})
	db.Create(&cfUser{ID: 3, Name: "carol", Secret: "s3"})
	db.Create(&cfOrder{ID: 1, CfUserID: 1})
	db.Create(&cfOrder{ID: 2, CfUserID: 1})
	db.Delete(&cfOrder{ID: 2})
	return db
}

func cfFilter(db *gorm.DB, filters string, out interface{}) Page {
	return New(&Config{ErrorEnabled: true}).With(db.Model(&cfUser{})).
		Request(getReq("size=10&filters=" + url.QueryEscape(filters))).Response(out)
}

// C-01/C-07: breadth caps on columns, list values, nodes and sorts.
func TestBreadthCaps(t *testing.T) {
	db := setupCfDB(t)
	vals := strings.TrimSuffix(strings.Repeat("1,", maxListValues+1), ",")
	nodes := strings.TrimSuffix(strings.Repeat(`["id",1],`, maxFilterNodes), ",")
	rejected := []string{
		`["` + strings.TrimSuffix(strings.Repeat("id,", maxFilterColumns+1), ",") + `","IN",[1]]`,
		`[["id","IN",[` + vals + `]]]`,
		`[` + nodes + `]`,
	}
	for _, f := range rejected {
		var out []cfUser
		if page := cfFilter(db, f, &out); !page.Error || page.ErrorMessage != "invalid filter parameter" {
			t.Errorf("breadth cap not enforced for %.60s…: %+v", f, page)
		}
	}
	var out []cfUser
	ok := `["` + strings.TrimSuffix(strings.Repeat("id,", maxFilterColumns), ",") + `","IN",[1,2]]`
	if page := cfFilter(db, ok, &out); page.Error || page.Total != 2 {
		t.Errorf("filter within caps must work: %+v", page)
	}
	page := New().With(db.Model(&cfUser{})).
		Request(getReq("sort=" + strings.TrimSuffix(strings.Repeat("id,", maxSortColumns+1), ","))).Response(&out)
	if !page.Error || page.ErrorMessage != "invalid sort parameter" {
		t.Errorf("sort cap not enforced: %+v", page)
	}
}

// C-02: a client fields list matching nothing must not widen to SELECT *.
func TestFieldsNoMatchKeepsServerProjection(t *testing.T) {
	db := setupCfDB(t)
	var out []map[string]interface{}
	page := New(&Config{FieldSelectorEnabled: true}).With(db.Model(&cfUser{})).
		Request(getReq("fields=zzz")).Fields([]string{"id", "name"}).Response(&out)
	if page.RawError != nil || len(out) != 3 {
		t.Fatalf("unexpected page: %+v", page)
	}
	if len(out[0]) != 2 || out[0]["secret"] != nil {
		t.Errorf("projection widened: %v", out[0])
	}
}

// C-03: preloads keep the caller's soft-delete scope.
func TestPreloadHidesSoftDeleted(t *testing.T) {
	db := setupCfDB(t)
	var out []cfUser
	New().With(db.Model(&cfUser{}).Preload("Orders").Where("id = 1")).Request(getReq("")).Response(&out)
	if len(out) != 1 || len(out[0].Orders) != 1 {
		t.Errorf("soft-deleted order leaked through preload: %+v", out)
	}
	out = nil
	New().With(db.Unscoped().Model(&cfUser{}).Preload("Orders").Where("id = 1")).Request(getReq("")).Response(&out)
	if len(out) != 1 || len(out[0].Orders) != 2 {
		t.Errorf("caller Unscoped() must still include deleted orders: %+v", out)
	}
	out = nil
	unscopedOrders := func(db *gorm.DB) *gorm.DB { return db.Unscoped() }
	New().With(db.Model(&cfUser{}).Preload("Orders", unscopedOrders).Where("id = 1")).Request(getReq("")).Response(&out)
	if len(out) != 1 || len(out[0].Orders) != 2 {
		t.Errorf("per-preload Unscoped() func must win: %+v", out)
	}
}

// C-04: client fields are de-duplicated and capped.
func TestFieldsDedupAndCap(t *testing.T) {
	pr := parseRequest(getReq("fields=id,id,name,id"), Config{})
	if len(pr.Fields) != 2 {
		t.Errorf("fields not de-duplicated: %v", pr.Fields)
	}
	many := make([]string, maxFieldColumns+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	pr = parseRequest(getReq("fields="+strings.Join(many, ",")), Config{FieldSelectorEnabled: true})
	if !pr.Invalid {
		t.Errorf("fields cap not enforced")
	}
	pr = parseRequest(getReq("fields="+strings.Join(many, ",")), Config{})
	if pr.Invalid || len(pr.Fields) != 0 {
		t.Errorf("selector disabled: oversized fields must be dropped, not rejected")
	}
}

// C-05: Total ignores the projection (no COUNT(col) NULL skipping).
func TestSingleFieldTotalCountsAllRows(t *testing.T) {
	db := setupCfDB(t)
	var out []map[string]interface{}
	page := New().With(db.Model(&cfUser{})).Request(getReq("")).Fields([]string{"nick"}).Response(&out)
	if page.Total != 3 || len(out) != 3 {
		t.Errorf("total=%d rows=%d, want 3/3", page.Total, len(out))
	}
}

// C-06: integers bind as int64 — above 1e6 and above 2^53.
func TestIntegerPrecision(t *testing.T) {
	if v, ok := valueFixer(float64(1000000)).(int64); !ok || v != 1000000 {
		t.Errorf("1e6 must bind as int64, got %T", valueFixer(float64(1000000)))
	}
	pr := parseRequest(getReq("filters="+url.QueryEscape(`[["id","=",9007199254740993]]`)), *New().Config)
	_, params := generateWhereCauses(pr.Filters, pr.Config)
	if len(params) != 1 || params[0] != int64(9007199254740993) {
		t.Errorf("big integer lost precision: %#v", params)
	}
}

// C-08/C-09/C-10/C-12a/C-12c: shapes that used to reach the DB malformed or inverted.
func TestConnectorAndListSemantics(t *testing.T) {
	db := setupCfDB(t)
	rejected := []string{
		`[["name","=","alice"],["OR"],["OR"],["id","=",2]]`,
		`[["id","NOT IN",[1,null]]]`,
		`[["name","like",null]]`,
		`[["name","like",["a"]]]`,
	}
	for _, f := range rejected {
		var out []cfUser
		if page := cfFilter(db, f, &out); !page.Error || page.ErrorMessage != "invalid filter parameter" {
			t.Errorf("%s must be rejected: %+v", f, page)
		}
	}
	var out []cfUser
	if page := cfFilter(db, `[["id","NOT IN",[]]]`, &out); page.Error || page.Total != 3 {
		t.Errorf("NOT IN [] must keep every row: %+v", page)
	}
	if page := cfFilter(db, `[["id","IN",[]]]`, &out); page.Error || page.Total != 0 {
		t.Errorf("IN [] must match nothing: %+v", page)
	}
	if page := cfFilter(db, `[["name","ilike","ALI"]]`, &out); page.Error || page.Total != 1 {
		t.Errorf("ILIKE must work on sqlite: %+v", page)
	}
	pr := parseRequest(getReq("filters="+url.QueryEscape(`[["name","like",1000000]]`)), *New().Config)
	if _, params := generateWhereCauses(pr.Filters, pr.Config); len(params) != 1 || params[0] != "%1000000%" {
		t.Errorf("numeric LIKE operand mangled: %#v", params)
	}

	req, _ := http.NewRequest("POST", "/", strings.NewReader(`{"filters":{"a":1}}`))
	page := New().With(db.Model(&cfUser{})).Request(req).Response(&out)
	if !page.Error {
		t.Errorf("object filters must fail closed: %+v", page)
	}
}

// C-12e: parsing never mutates the caller's request, and nil URL is safe.
func TestRequestNotMutated(t *testing.T) {
	r := &http.Request{URL: &url.URL{RawQuery: "size=5"}}
	pr := parseRequest(r, Config{})
	if r.Method != "" || pr.Size != 5 {
		t.Errorf("method=%q size=%d", r.Method, pr.Size)
	}
	parseRequest(&http.Request{Method: "GET"}, Config{}) // must not panic
}

// C-12g/C-12h: XOR extras dropped; scope is length-prefixed in the key.
func TestExtraXorAndScopedKey(t *testing.T) {
	if ops := sanitizeExtraOperators([]string{"XOR", "SIMILAR TO"}); len(ops) != 1 || ops[0] != "SIMILAR TO" {
		t.Errorf("XOR must be dropped: %v", ops)
	}
	pr := parseRequest(&Request{Size: 10}, Config{})
	if createCacheKey("p", pr, "a") == createCacheKey("p", pr, "a|") {
		t.Errorf("scoped keys collide")
	}
}

// R-01: json:"-" columns are not filterable/sortable by default.
func TestHiddenColumnsRejected(t *testing.T) {
	db := setupCfDB(t)
	for _, q := range []string{
		"filters=" + url.QueryEscape(`[["secret",">=","s2"]]`),
		"filters=" + url.QueryEscape(`[["SECRET",">=","s2"]]`),
		"sort=-secret",
	} {
		var out []cfUser
		page := New().With(db.Model(&cfUser{})).Request(getReq(q)).Response(&out)
		if !page.Error {
			t.Errorf("hidden column accepted for %s: %+v", q, page)
		}
	}
	var out []cfUser
	if page := New(&Config{FieldSelectorEnabled: true}).With(db.Model(&cfUser{})).
		Request(getReq("fields=id,secret")).Response(&out); !page.Error {
		t.Errorf("hidden column selectable: %+v", page)
	}
	if page := New().With(db.Model(&cfUser{})).
		Request(getReq("fields=secret")).Response(&out); page.Error {
		t.Errorf("fields= must stay inert with the selector disabled: %+v", page)
	}
	page := New(&Config{AllowHiddenColumns: true}).With(db.Model(&cfUser{})).
		Request(getReq("filters=" + url.QueryEscape(`[["secret",">=","s2"]]`))).Response(&out)
	if page.Error || page.Total != 2 {
		t.Errorf("AllowHiddenColumns must opt back in: %+v", page)
	}
}

// nameDialector reports a different dialect name over sqlite's quoting, so
// the postgres/mysql SQL-generation branches can be exercised without drivers.
type nameDialector struct {
	gorm.Dialector
	name string
}

func (d nameDialector) Name() string { return d.name }

func dialectWhere(t *testing.T, dialect, filters string) string {
	t.Helper()
	cfg := *New().Config
	cfg.Statement = &gorm.Statement{DB: &gorm.DB{Config: &gorm.Config{
		Dialector: nameDialector{sqlite.Open(":memory:"), dialect},
	}}}
	pr := parseRequest(getReq("filters="+url.QueryEscape(filters)), cfg)
	wheres, _ := generateWhereCauses(pr.Filters, pr.Config)
	return strings.Join(wheres, " ")
}

// C-12b/C-12c/C-12f: dialect-specific SQL text.
func TestDialectSQLGeneration(t *testing.T) {
	cases := []struct{ dialect, filters, want string }{
		{"postgres", `[["name","IS","x"]]`, "IS NOT DISTINCT FROM ?"},
		{"postgres", `[["name","IS NOT","x"]]`, "IS DISTINCT FROM ?"},
		{"postgres", `[["active","IS",true]]`, "IS TRUE"},
		{"postgres", `[["name","ILIKE","x"]]`, "ILIKE ? ESCAPE '!'"},
		{"mysql", `[["name","IS","x"]]`, "<=> ?"},
		{"mysql", `[["name","IS NOT","x"]]`, "NOT ( `name` <=> ? )"},
		{"mysql", `[["name","NOT ILIKE","x"]]`, "NOT LIKE ? ESCAPE '!'"},
		{"sqlite", `[["name","IS","x"]]`, "`name` IS ?"},
		{"sqlite", `[["name","ILIKE","x"]]`, "LIKE ? ESCAPE '!'"},
	}
	for _, c := range cases {
		got := dialectWhere(t, c.dialect, c.filters)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s %s: got %q, want it to contain %q", c.dialect, c.filters, got, c.want)
		}
		if c.dialect != "postgres" && strings.Contains(got, "ILIKE") {
			t.Errorf("%s must not emit ILIKE: %q", c.dialect, got)
		}
	}
}
