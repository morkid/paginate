package paginate

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/goccy/go-json"

	"github.com/iancoleman/strcase"
	"github.com/morkid/gocache"
	"gorm.io/gorm"

	"github.com/valyala/fasthttp"
)

// maxRequestBodySize caps POST body reads to prevent memory exhaustion.
const maxRequestBodySize = 1 << 20 // 1 MB

// defaultMaxPageSize caps client-provided page sizes when Config.MaxSize is unset.
const defaultMaxPageSize = 100

// Breadth caps (C-01/C-04/C-07): depth alone is capped by maxFilterDepth, so
// these bound the work a single request can cause. Multi-column filters fan a
// shared value out per column, so columns × list values must stay small.
const (
	maxFilterColumns = 8    // columns in one "a,b,c" filter reference
	maxListValues    = 1000 // values in one IN / NOT IN list
	maxFilterNodes   = 256  // predicate + connector nodes in a whole tree
	maxSortColumns   = 8    // ORDER BY terms
	maxFieldColumns  = 128  // distinct entries in a client fields list
)

// likeEscapeChar is used for every dialect (C-12f): unlike a backslash it has
// no meaning inside SQL string literals, so the ESCAPE clause is portable
// across standard_conforming_strings / NO_BACKSLASH_ESCAPES settings.
const likeEscapeChar = "!"

// defaultUnmarshalPtr identifies the package-default decoder so request
// parsing can switch to json.Number decoding (C-06) only when the caller has
// not supplied a custom JSONUnmarshal.
var defaultUnmarshalPtr = reflect.ValueOf(json.Unmarshal).Pointer()

// unmarshalRequest decodes untrusted request JSON. With the default decoder,
// numbers become json.Number so integers above 2^53 keep full precision.
func unmarshalRequest(config Config, data []byte, v interface{}) error {
	if config.JSONUnmarshal != nil && reflect.ValueOf(config.JSONUnmarshal).Pointer() != defaultUnmarshalPtr {
		return config.JSONUnmarshal(data, v)
	}
	if !hasLongDigitRun(data) {
		// float64 is exact for up to 15 digits: skip the costlier Decoder.
		return json.Unmarshal(data, v)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("paginate: unexpected trailing JSON data")
		}
		return err
	}
	return nil
}

// hasLongDigitRun reports whether data holds 16+ consecutive digits — the
// only case where float64 decoding can lose integer precision.
func hasLongDigitRun(data []byte) bool {
	run := 0
	for _, c := range data {
		if c >= '0' && c <= '9' {
			run++
			if run >= 16 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// logError logs an error on one line: DB/JSON errors can echo client input,
// and raw newlines would let a client forge log entries (C-12i).
func logError(err error) {
	if err == nil {
		return
	}
	log.Println(strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(err.Error()))
}

// allowedOperators is the strict comparison-operator allowlist. Anything not in
// this set (or Config.ExtraOperators) is rejected before a query is built, so
// untrusted input can never reach the WHERE clause as SQL syntax.
var allowedOperators = map[string]bool{
	"=": true, "!=": true, "<>": true, "<": true, ">": true, "<=": true, ">=": true,
	"LIKE": true, "NOT LIKE": true, "ILIKE": true, "NOT ILIKE": true,
	"IN": true, "NOT IN": true, "BETWEEN": true, "IS": true, "IS NOT": true,
}

// hoisted regexes — compiled once at init instead of per request
var smartSearchRe = regexp.MustCompile(`[\s]+`)

// cacheScopeWarnOnce rate-limits the missing-CacheScope advisory to one log.
var cacheScopeWarnOnce sync.Once

// extraOpWarnOnce rate-limits invalid-ExtraOperators drop logs to one line
// (configs bypassing New() would otherwise re-log on every request, V3-02).
var extraOpWarnOnce sync.Once

// read-only dialect wrapper maps shared across requests
var (
	defaultLikeWrappers = map[string]string{
		"sqlite":   "LOWER(%s)",
		"mysql":    "LOWER(%s)",
		"postgres": "LOWER((%s)::text)",
	}
	noLikeWrappers = map[string]string{
		"sqlite":   "%s",
		"mysql":    "%s",
		"postgres": "(%s)::text",
	}
)

// normalizeOperator upper-cases and validates a comparison operator against
// the allowlist. Returns the normalized operator and whether it is permitted.
func normalizeOperator(operator string, config Config) (string, bool) {
	op := strings.ToUpper(strings.TrimSpace(operator))
	return op, isAllowedOperator(op, config)
}

// isAllowedOperator reports whether op is in the built-in allowlist or the
// config's extra operators.
func isAllowedOperator(op string, config Config) bool {
	if allowedOperators[op] {
		return true
	}
	for _, extra := range config.ExtraOperators {
		if strings.EqualFold(strings.TrimSpace(extra), op) {
			return true
		}
	}
	return false
}

// isConnector reports whether op is a safe logical connector (AND/OR).
func isConnector(op string) bool {
	op = strings.ToUpper(strings.TrimSpace(op))
	return op == "AND" || op == "OR"
}

// isIdentSegment reports whether s is a plain identifier: [A-Za-z_][A-Za-z0-9_]*.
func isIdentSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// validColumn validates a comma-separated, dot-qualified column reference
// without allocating (P-02). Config.ColumnValidator, when set, takes precedence.
func validColumn(column string, config Config) bool {
	if column == "" {
		return false
	}
	if config.ColumnValidator != nil {
		return config.ColumnValidator(column)
	}
	segStart := 0
	for i := 0; i <= len(column); i++ {
		if i == len(column) || column[i] == ',' || column[i] == '.' {
			if !isIdentSegment(column[segStart:i]) {
				return false
			}
			segStart = i + 1
		}
	}
	return true
}

// hasInvalidFilter reports whether any node in the filter tree was rejected
// (operator/column/shape validation) or falls outside a configured allowlist.
func hasInvalidFilter(f pageFilters) bool {
	if f.Invalid {
		return true
	}
	if f.Single && !f.IsOperator && f.Column != "" && len(f.Fields) > 0 && !contains(f.Fields, f.Column) {
		return true
	}
	switch v := f.Value.(type) {
	case []pageFilters:
		for i := range v {
			if hasInvalidFilter(v[i]) {
				return true
			}
		}
	case pageFilters:
		return hasInvalidFilter(v)
	}
	return false
}

// stampFilterFields propagates a field allowlist to every node of the tree.
func stampFilterFields(f *pageFilters, fields []string) {
	if f == nil || len(fields) == 0 {
		return
	}
	f.Fields = fields
	switch v := f.Value.(type) {
	case []pageFilters:
		for i := range v {
			stampFilterFields(&v[i], fields)
		}
	case pageFilters:
		child := v
		stampFilterFields(&child, fields)
		f.Value = child
	}
}

// countFilterNodes counts every node of a filter tree (C-07 breadth cap).
func countFilterNodes(f pageFilters) int {
	n := 1
	switch v := f.Value.(type) {
	case []pageFilters:
		for i := range v {
			n += countFilterNodes(v[i])
		}
	case pageFilters:
		n += countFilterNodes(v)
	}
	return n
}

// hiddenColumns returns the lower-cased DB names of destination fields tagged
// `json:"-"` (R-01): those columns are invisible in responses, so filtering or
// sorting on them would turn `total` into a blind extraction oracle.
// Destinations without a gorm schema (e.g. maps) yield nil.
func hiddenColumns(db *gorm.DB, dest interface{}) map[string]struct{} {
	if db == nil || dest == nil {
		return nil
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(dest); err != nil || stmt.Schema == nil {
		return nil
	}
	var hidden map[string]struct{}
	for _, field := range stmt.Schema.Fields {
		if field.DBName == "" || field.Tag.Get("json") != "-" {
			continue
		}
		if hidden == nil {
			hidden = map[string]struct{}{}
		}
		hidden[strings.ToLower(field.DBName)] = struct{}{}
	}
	return hidden
}

// isHiddenColumn compares case-insensitively: sqlite/mysql resolve quoted
// identifiers without regard to case, so "PASSWORD" must not slip through.
func isHiddenColumn(column string, hidden map[string]struct{}) bool {
	if len(hidden) == 0 {
		return false
	}
	_, ok := hidden[strings.ToLower(column)]
	return ok
}

// hasHiddenColumn reports whether any predicate in the tree uses a hidden column.
func hasHiddenColumn(f pageFilters, hidden map[string]struct{}) bool {
	if len(hidden) == 0 {
		return false
	}
	if f.Single && !f.IsOperator && isHiddenColumn(f.Column, hidden) {
		return true
	}
	switch v := f.Value.(type) {
	case []pageFilters:
		for i := range v {
			if hasHiddenColumn(v[i], hidden) {
				return true
			}
		}
	case pageFilters:
		return hasHiddenColumn(v, hidden)
	}
	return false
}

// markStrictInvalid flags nodes that would produce no WHERE fragments when
// Config.StrictFilters is enabled (N-11). A zero node with nil Value is the
// legitimate "no filters" state and stays valid.
func markStrictInvalid(f *pageFilters) {
	markStrictNode(f, false)
}

// markStrictNode flags garbage nodes when Config.StrictFilters is enabled.
// nested distinguishes the legitimate root "no filters" state (Value == nil at
// the top) from zero nodes sitting inside a group — the V3-01/AGY3-01 gap.
func markStrictNode(f *pageFilters, nested bool) {
	if f == nil || f.Invalid {
		return
	}
	if f.Single && !f.IsOperator {
		if f.Column == "" {
			f.Invalid = true
		}
		return
	}
	if f.IsOperator {
		return
	}
	if f.Value == nil && f.Column == "" && nested {
		f.Invalid = true // zero group member, e.g. filter shape "[[]]"
		return
	}
	switch v := f.Value.(type) {
	case []pageFilters:
		if len(v) == 0 && f.Column == "" && f.Value != nil {
			f.Invalid = true
			return
		}
		for i := range v {
			markStrictNode(&v[i], true)
		}
	case pageFilters:
		child := v
		markStrictNode(&child, true)
		f.Value = child
	default:
		// group-position node holding a non-filter value (dropped element)
		if f.Value != nil && f.Column == "" {
			f.Invalid = true
		}
	}
}

// escapeLikeValue prefixes SQL LIKE wildcards and the (single-byte) escape
// character itself in a single pass (replaces the former per-value regexp, P-04).
// Returns the input unchanged — zero allocations — when nothing needs escaping.
func escapeLikeValue(value, escapeChar string) string {
	if escapeChar == "" || value == "" {
		return value
	}
	esc := escapeChar[0]
	needsEscape := false
	for i := 0; i < len(value); i++ {
		if c := value[i]; c == '%' || c == '_' || c == esc {
			needsEscape = true
			break
		}
	}
	if !needsEscape {
		return value
	}
	var b strings.Builder
	b.Grow(len(value) + 8)
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '%' || c == '_' || c == esc {
			b.WriteString(escapeChar)
		}
		b.WriteByte(c)
	}
	return b.String()
}

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
		adapter := p.Config.CacheAdapter
		for i := range keyPrefixes {
			// N-07b: exact-scope clearing — "user" must not clear "user1:..."
			// keys (adapters match by plain string prefix).
			prefix := keyPrefixes[i]
			if !strings.HasSuffix(prefix, ":") {
				prefix += ":"
			}
			if err := adapter.ClearPrefix(prefix); nil != err {
				log.Println(err)
			}
		}
	}
}

// ClearAllCache clear all existing cache
func (p Pagination) ClearAllCache() {
	if nil != p.Config && nil != p.Config.CacheAdapter {
		adapter := p.Config.CacheAdapter
		if err := adapter.ClearAll(); nil != err {
			log.Println(err)
		}
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
	page := Page{}
	if r.Pagination == nil {
		page.Error = true
		page.ErrorMessage = "invalid pagination instance"
		page.RawError = errors.New("invalid pagination instance")
		return page
	}
	query := r.Statement
	if query == nil || query.Statement == nil {
		page.Error = true
		page.ErrorMessage = "invalid pagination statement"
		page.RawError = errors.New("invalid pagination statement")
		return page
	}

	// Copy the shared config: concurrent requests must never mutate the
	// caller's *Pagination (data race on Config.Statement and friends).
	var cfg Config
	if r.Pagination.Config != nil {
		cfg = *r.Pagination.Config
	}
	if cfg.JSONMarshal == nil {
		cfg.JSONMarshal = json.Marshal
	}
	if cfg.JSONUnmarshal == nil {
		cfg.JSONUnmarshal = json.Unmarshal
	}
	cfg.Statement = query.Statement
	if cfg.DefaultSize == 0 {
		cfg.DefaultSize = 10
	}
	if cfg.PageStart < 0 {
		cfg.PageStart = 0
	}

	wrappers := defaultLikeWrappers
	if cfg.LikeAsIlikeDisabled {
		wrappers = noLikeWrappers
	}
	if cfg.FieldWrapper == "" && cfg.ValueWrapper == "" {
		cfg.FieldWrapper = "LOWER(%s)"
		if wrapper, ok := wrappers[query.Dialector.Name()]; ok {
			cfg.FieldWrapper = wrapper
		}
	}

	for _, f := range r.fieldList {
		// N-09: server-side Fields() entries get the same identifier
		// validation as request fields before any quoting.
		if !validColumn(f, cfg) {
			page.Error = true
			page.ErrorMessage = "invalid field parameter"
			page.RawError = errors.New("invalid field parameter")
			return page
		}
	}

	pr := parseRequest(r.Request, cfg)
	if pr.Invalid {
		// Fail closed on parse-level rejections (malformed/oversized request
		// bodies) — always visible, like causes.Invalid below.
		page.Error = true
		page.ErrorMessage = pr.ErrorMessage
		if page.ErrorMessage == "" {
			page.ErrorMessage = "invalid request"
		}
		page.RawError = errors.New(page.ErrorMessage)
		return page
	}
	// N-04: the server-side Fields() list is always authoritative; the
	// request's own ?fields= only constrains filters/sorts when field
	// selection is actually enabled (restores v1.1.11 behavior otherwise).
	allowlist := r.fieldList
	if len(allowlist) == 0 && cfg.FieldSelectorEnabled {
		allowlist = pr.Filters.Fields
	}
	if len(allowlist) > 0 {
		stampFilterFields(&pr.Filters, allowlist)
	} else {
		pr.Filters.Fields = nil // clear stale top-node value set by createFilters
	}
	if cfg.StrictFilters {
		markStrictInvalid(&pr.Filters)
	}
	causes := createCauses(pr)
	if !causes.Invalid && !cfg.AllowHiddenColumns {
		// R-01: columns hidden from the JSON output must not be filterable,
		// sortable or selectable — `total` would leak them bit by bit.
		if hidden := hiddenColumns(query.Statement.DB, res); len(hidden) > 0 {
			if hasHiddenColumn(pr.Filters, hidden) {
				causes.Invalid = true
				causes.ErrorMessage = "invalid filter parameter"
			}
			for _, so := range pr.Sorts {
				if !causes.Invalid && isHiddenColumn(so.Column, hidden) {
					causes.Invalid = true
					causes.ErrorMessage = "invalid sort parameter"
				}
			}
			for _, f := range pr.Fields {
				// fields= is inert unless the selector is enabled (N-04).
				if cfg.FieldSelectorEnabled && !causes.Invalid && isHiddenColumn(f, hidden) {
					causes.Invalid = true
					causes.ErrorMessage = "invalid field parameter"
				}
			}
		}
	}
	if causes.Invalid {
		// N-05: validation rejections are always visible (fixed, non-sensitive
		// message) — independent of ErrorEnabled, which only gates raw DB text.
		page.Error = true
		page.ErrorMessage = causes.ErrorMessage
		page.RawError = errors.New(causes.ErrorMessage)
		return page
	}

	cKey := ""
	var adapter gocache.AdapterInterface
	var hasAdapter bool = false

	if nil != cfg.CacheAdapter {
		if cfg.CacheScope == nil {
			// N-07a: shared adapters without a scope key share pages across
			// users/tenants — warn once so misconfiguration is visible.
			cacheScopeWarnOnce.Do(func() {
				log.Println("paginate: CacheAdapter set without Config.CacheScope — " +
					"cached pages may be shared across users/tenants; set CacheScope for shared adapters")
			})
		}
		scope := ""
		if cfg.CacheScope != nil {
			scope = cfg.CacheScope()
		}
		cKey = createCacheKey(r.cachePrefix, pr, scope)
		adapter = cfg.CacheAdapter
		hasAdapter = true
		if cKey != "" && adapter.IsValid(cKey) {
			if cache, err := adapter.Get(cKey); nil == err {
				if cached, ok := restoreCachedPage(cache, cfg.JSONUnmarshal, res); ok {
					return cached
				}
			}
		}
	}

	dbs := query.Statement.DB.Session(&gorm.Session{NewDB: true})
	var selects []string
	addSelect := func(field string) {
		fname := query.Statement.Quote("s." + fieldName(field))
		if !contains(selects, fname) {
			selects = append(selects, fname)
		}
	}
	if len(r.fieldList) > 0 {
		if len(pr.Fields) > 0 && cfg.FieldSelectorEnabled {
			for i := range pr.Fields {
				if contains(r.fieldList, pr.Fields[i]) {
					addSelect(pr.Fields[i])
				}
			}
		}
		// C-02: a client selection that matches nothing must fall back to the
		// server list — never to an unrestricted SELECT *.
		if len(selects) == 0 {
			for i := range r.fieldList {
				addSelect(r.fieldList[i])
			}
		}
	} else if len(pr.Fields) > 0 && cfg.FieldSelectorEnabled {
		for i := range pr.Fields {
			addSelect(pr.Fields[i])
		}
	}

	base := dbs.
		Unscoped().
		Table("(?) AS s", query)

	if len(causes.Params) > 0 || len(causes.WhereString) > 0 {
		base = base.Where(causes.WhereString, causes.Params...)
	}

	// C-05: count BEFORE the projection is added — with exactly one selected
	// column gorm emits COUNT(col), which skips NULLs and under-reports Total.
	// Count restores the statement afterwards, so the same chain continues.
	countRes := base.Count(&page.Total)
	page.RawError = countRes.Error

	if countRes.Error != nil {
		logError(countRes.Error)
		if cfg.ErrorEnabled {
			page.Error = true
			page.ErrorMessage = "database error"
		}
	}

	result := countRes
	if len(selects) > 0 {
		result = result.Select(selects)
	}
	result = result.
		Limit(int(causes.Limit)).
		Offset(int(causes.Offset))

	if nil != query.Statement.Preloads {
		for table, args := range preloadsWithScope(query.Statement.Preloads, query.Statement.Unscoped) {
			result = result.Preload(table, args...)
		}
	}
	if len(causes.Sorts) > 0 {
		for _, sort := range causes.Sorts {
			result = result.Order(sort.Column + " " + sort.Direction)
		}
	}

	rs := countRes
	if countRes.Error == nil {
		rs = result.Find(res)
		page.RawError = rs.Error
		if rs.Error != nil {
			logError(rs.Error)
			if cfg.ErrorEnabled {
				page.Error = true
				page.ErrorMessage = "database error"
			}
		}
	}

	page.Items = res
	f := float64(page.Total) / float64(causes.Limit)
	if math.Mod(f, 1.0) > 0 {
		f = f + 1
	}
	f = math.Max(f, 1)

	page.TotalPages = int64(f)
	page.MaxPage = page.TotalPages - 1 + cfg.PageStart
	page.Page = pr.Page
	page.Size = pr.Size
	page.Visible = 0
	if rs.Error == nil && countRes.Error == nil {
		page.Visible = rs.RowsAffected
	}

	if page.Total < 1 {
		page.MaxPage = cfg.PageStart
		page.TotalPages = 0
	}
	page.First = causes.Offset < 1
	page.Last = page.Page >= page.MaxPage

	if hasAdapter && cKey != "" && !page.Error && nil == page.RawError {
		if cache, err := cfg.JSONMarshal(page); nil == err {
			if err := adapter.Set(cKey, string(cache)); err != nil {
				log.Println(err)
			}
		}
	}

	return page
}

// restoreCachedPage decodes a cached page: metadata into the Page envelope and
// `items` back into the caller's typed destination, so warm and cold cache
// responses have identical types.
func restoreCachedPage(cache string, unmarshal func([]byte, interface{}) error, res interface{}) (Page, bool) {
	var cached struct {
		Page
		RawItems json.RawMessage `json:"items"`
	}
	if err := unmarshal([]byte(cache), &cached); err != nil {
		return Page{}, false
	}
	if len(cached.RawItems) > 0 {
		if err := unmarshal(cached.RawItems, res); err != nil {
			return Page{}, false
		}
	}
	page := cached.Page
	page.Items = res
	return page, true
}

// preloadsWithScope re-applies the caller's soft-delete scope to every preload
// (C-03). The outer query must run Unscoped (the soft-delete clause cannot
// target the "s" alias), and gorm copies that flag into each preload query.
// The prepended condition runs inside the preload query and restores the
// caller's own setting. Every ancestor path ("a" for "a.b") gets it too, so
// nested preloads inherit the corrected flag.
func preloadsWithScope(preloads map[string][]interface{}, unscoped bool) map[string][]interface{} {
	restore := func(db *gorm.DB) *gorm.DB {
		db.Statement.Unscoped = unscoped
		return db
	}
	out := make(map[string][]interface{}, len(preloads))
	for name, args := range preloads {
		// restore runs FIRST so the caller's own preload funcs (e.g. a
		// per-preload Unscoped()) still have the final say.
		withScope := make([]interface{}, 0, len(args)+1)
		withScope = append(withScope, restore)
		out[name] = append(withScope, args...)
	}
	for name := range preloads {
		for i := 0; i < len(name); i++ {
			if name[i] != '.' {
				continue
			}
			if _, ok := out[name[:i]]; !ok {
				out[name[:i]] = []interface{}{restore}
			}
		}
	}
	return out
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

		return &Pagination{Config: defaultConfig(config)}
	}

	return &Pagination{Config: defaultConfig(nil)}
}

// parseRequest func
func parseRequest(r interface{}, config Config) pageRequest {
	pr := pageRequest{
		Config: *defaultConfig(&config),
	}
	switch req := r.(type) {
	case http.Request:
		parsingNetHTTPRequest(&req, &pr)
	case *http.Request:
		parsingNetHTTPRequest(req, &pr)
	case *fasthttp.Request:
		parsingFastHTTPRequest(req, &pr)
	case *Request:
		parsingQueryString(req, &pr)
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
		if e := unmarshalRequest(p.Config, []byte(s), &iface); nil == e && len(iface) > 0 {
			p.Filters = arrayToFilter(iface, p.Config)
		} else if s != "" && nil != e {
			// N-02: non-empty filters input that fails to parse must fail
			// closed instead of silently dropping all filtering.
			p.Filters.Invalid = true
		}
		p.Filters.Fields = p.Fields
	} else if filterParams != nil {
		// C-10: objects, numbers and booleans are not filter trees — fail
		// closed like an unparsable string instead of dropping all filtering.
		p.Filters.Invalid = true
		p.Filters.Fields = p.Fields
	}
	if countFilterNodes(p.Filters) > maxFilterNodes {
		p.Filters.Invalid = true // C-07: breadth cap
	}
}

// createCauses func
func createCauses(p pageRequest) requestQuery {
	query := requestQuery{}
	wheres, params := generateWhereCauses(p.Filters, p.Config)
	sorts := []sortOrder{}

	for _, so := range p.Sorts {
		// Strict column validation (F-02/F-03): reject before quoting/concat.
		if !validColumn(so.Column, p.Config) {
			query.Invalid = true
			query.ErrorMessage = "invalid sort parameter"
			continue
		}
		// Enforce the Fields() allowlist on sorts (F-08).
		if len(p.Filters.Fields) > 0 && !contains(p.Filters.Fields, so.Column) {
			query.Invalid = true
			query.ErrorMessage = "invalid sort parameter"
			continue
		}
		so.Column = fieldName(so.Column)
		if nil != p.Config.Statement {
			so.Column = p.Config.Statement.Quote(so.Column)
		}
		sorts = append(sorts, so)
	}

	if hasInvalidFilter(p.Filters) {
		query.Invalid = true
		if query.ErrorMessage == "" {
			query.ErrorMessage = "invalid filter parameter"
		}
	}
	// N-06: a request whose entire filter is a bare connector has no predicate
	// to attach it to — reject instead of emitting raw "WHERE OR".
	if p.Filters.IsOperator && p.Filters.Single {
		query.Invalid = true
		if query.ErrorMessage == "" {
			query.ErrorMessage = "invalid filter parameter"
		}
	}

	// Overflow-safe limit/offset (F-04/F-13), then clamp to int range so
	// int64→int casts on 32-bit platforms cannot wrap to a negative offset
	// (gorm treats negative offset as "cancel", which would return page 0).
	size := p.Size
	if size <= 0 {
		size = 10
	}
	pageNum := p.Page - p.Config.PageStart
	if pageNum < 0 {
		pageNum = 0
	}
	var offset int64
	if pageNum > 0 && size > 0 && pageNum > math.MaxInt64/size {
		offset = math.MaxInt64 // far beyond any result set: empty page, no wrap
	} else {
		offset = pageNum * size
	}
	if strconv.IntSize == 32 {
		if offset > math.MaxInt32 {
			offset = math.MaxInt32
		}
		if size > math.MaxInt32 {
			size = math.MaxInt32
		}
	}

	query.Limit = size
	query.Offset = offset
	query.Wheres = wheres
	query.WhereString = strings.Join(wheres, " ")
	query.Sorts = sorts
	query.Params = params

	return query
}

// parsingNetHTTPRequest func
func parsingNetHTTPRequest(r *http.Request, p *pageRequest) {
	param := &Request{}
	method := strings.ToUpper(r.Method)
	if method == "" {
		method = "GET" // net/http convention; never written back to r
	}
	if method == "POST" {
		if nil != r.Body {
			b, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
			defer r.Body.Close()
			switch {
			case nil != err:
				// fail closed: unreadable body must not degrade to defaults
				p.Invalid = true
				p.ErrorMessage = "invalid request body"
			case len(b) > maxRequestBodySize:
				log.Println("paginate: request body exceeds limit")
				p.Invalid = true
				p.ErrorMessage = "request body too large"
			case len(bytes.TrimSpace(b)) == 0:
				// empty body == absent parameters (legitimate)
			default:
				if !p.Config.CustomParamEnabled {
					var postData Request
					if err := unmarshalRequest(p.Config, b, &postData); nil == err {
						param = &postData
					} else {
						logError(err)
						// fail closed: malformed JSON must not drop filters
						p.Invalid = true
						p.ErrorMessage = "invalid request body"
					}
				} else {
					var postData map[string]string
					if err := unmarshalRequest(p.Config, b, &postData); nil == err {
						generateParams(param, p.Config, func(key string) string {
							value, exists := postData[key]
							if !exists {
								value = ""
							}
							return value
						})
					} else {
						logError(err)
						p.Invalid = true
						p.ErrorMessage = "invalid request body"
					}
				}
			}
		}
	} else if method == "GET" && r.URL != nil {
		query := r.URL.Query()
		if !p.Config.CustomParamEnabled {
			param.Size, _ = strconv.ParseInt(query.Get("size"), 10, 64)
			param.Page, _ = strconv.ParseInt(query.Get("page"), 10, 64)
			param.Sort = query.Get("sort")
			param.Order = query.Get("order")
			param.Filters = query.Get("filters")
			param.Fields = strings.Split(query.Get("fields"), ",")
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
	param := &Request{}
	if r.Header.IsPost() {
		b := r.Body()
		switch {
		case len(b) > maxRequestBodySize:
			log.Println("paginate: request body exceeds limit")
			p.Invalid = true
			p.ErrorMessage = "request body too large"
		case len(bytes.TrimSpace(b)) == 0:
			// empty body == absent parameters (legitimate)
		default:
			if !p.Config.CustomParamEnabled {
				var postData Request
				if err := unmarshalRequest(p.Config, b, &postData); nil == err {
					param = &postData
				} else {
					logError(err)
					// fail closed: malformed JSON must not drop filters
					p.Invalid = true
					p.ErrorMessage = "invalid request body"
				}
			} else {
				var postData map[string]string
				if err := unmarshalRequest(p.Config, b, &postData); nil == err {
					generateParams(param, p.Config, func(key string) string {
						value, exists := postData[key]
						if !exists {
							value = ""
						}
						return value
					})
				} else {
					logError(err)
					p.Invalid = true
					p.ErrorMessage = "invalid request body"
				}
			}
		}
	} else if r.Header.IsGet() {
		query := r.URI().QueryArgs()
		if !p.Config.CustomParamEnabled {
			param.Size, _ = strconv.ParseInt(string(query.Peek("size")), 10, 64)
			param.Page, _ = strconv.ParseInt(string(query.Peek("page")), 10, 64)
			param.Sort = string(query.Peek("sort"))
			param.Order = string(query.Peek("order"))
			param.Filters = string(query.Peek("filters"))
			param.Fields = strings.Split(string(query.Peek("fields")), ",")
		} else {
			generateParams(param, p.Config, func(key string) string {
				return string(query.Peek(key))
			})
		}
	}

	parsingQueryString(param, p)
}

func parsingQueryString(param *Request, p *pageRequest) {
	// N-03: cap the client-provided size FIRST, then apply the server default
	// so Config.DefaultSize above MaxSize is honored (not silently clamped).
	maxSize := p.Config.MaxSize
	if maxSize <= 0 {
		maxSize = defaultMaxPageSize
	}
	p.Size = param.Size
	if p.Size > maxSize {
		p.Size = maxSize
	}
	if p.Size <= 0 {
		if p.Config.DefaultSize > 0 {
			p.Size = p.Config.DefaultSize
		} else {
			p.Size = 10
		}
	}

	p.Page = param.Page
	if p.Page < p.Config.PageStart {
		p.Page = p.Config.PageStart
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
		if len(p.Sorts) > maxSortColumns {
			p.Invalid = true // C-07: ORDER BY breadth cap
			p.ErrorMessage = "invalid sort parameter"
		}
	}

	if len(param.Fields) > 0 {
		// Strict identifier validation instead of the permissive A-z regex (F-09/F-17),
		// de-duplicated and capped so later allowlist scans stay linear (C-04).
		seen := make(map[string]struct{}, len(param.Fields))
		for _, field := range param.Fields {
			if _, dup := seen[field]; dup || !validColumn(field, p.Config) {
				continue
			}
			seen[field] = struct{}{}
			p.Fields = append(p.Fields, field)
		}
		if len(p.Fields) > maxFieldColumns {
			if p.Config.FieldSelectorEnabled {
				p.Invalid = true
				p.ErrorMessage = "invalid field parameter"
			}
			p.Fields = nil // inert input is dropped, not rejected (N-04)
		}
	}

	createFilters(param.Filters, p)
}

func generateParams(param *Request, config Config, getValue func(string) string) {
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
	param.Page, _ = strconv.ParseInt(findValue(config.PageParams, "page"), 10, 64)
	param.Size, _ = strconv.ParseInt(findValue(config.SizeParams, "size"), 10, 64)
	param.Order = findValue(config.OrderParams, "order")
	param.Filters = findValue(config.FilterParams, "filters")
	param.Fields = strings.Split(findValue(config.FieldsParams, "fields"), ",")
}

// maxFilterDepth caps filter-tree nesting (V4-01): realistic filters nest
// ~4-6 levels; a crafted 200+ level payload would otherwise build a WHERE
// that overflows the database's expression parser (availability DoS).
const maxFilterDepth = 16

func arrayToFilter(arr []interface{}, config Config) pageFilters {
	return arrayToFilterDepth(arr, config, 0)
}

func arrayToFilterDepth(arr []interface{}, config Config, depth int) pageFilters {
	if depth > maxFilterDepth {
		return pageFilters{Invalid: true} // rejected fail-closed upstream (V4-01)
	}
	filters := pageFilters{
		Single: false,
	}

	arrayLen := len(arr)
	defaultOperator := config.Operator
	if defaultOperator == "" {
		defaultOperator = "OR"
	}
	if !isConnector(defaultOperator) {
		defaultOperator = "OR"
	}

	if len(arr) > 0 {
		subFilters := []pageFilters{}
		stray := false
		for k, i := range arr {
			iface, ok := i.([]interface{})
			if ok && !filters.Single {
				subFilters = append(subFilters, arrayToFilterDepth(iface, config, depth+1))
			} else if arrayLen == 1 {
				// Standalone operator element: logical connector only (F-01).
				operator, ok := i.(string)
				if ok {
					op := strings.ToUpper(strings.TrimSpace(operator))
					filters.Operator = op
					filters.IsOperator = true
					filters.Single = true
					if !isConnector(op) {
						filters.Invalid = true
					}
				} else {
					filters.Invalid = true // N-06: non-string connector
				}
			} else if arrayLen == 2 {
				if k == 0 {
					if column, ok := i.(string); ok {
						filters.Column = column
						filters.Operator = "="
						filters.Single = true
						if !validColumn(column, config) {
							filters.Invalid = true
						}
					} else {
						filters.Invalid = true // N-06: non-string column
					}
				} else if k == 1 {
					if filters.Column == "" && len(subFilters) > 0 {
						stray = true // V3-01: value alongside group members
					}
					filters.Value = i
					if nil == i {
						filters.Operator = "IS"
					}
					if strings.Contains(filters.Column, ",") {
						subFilters = filterToSubFilter(&filters, i, config, depth)
						continue
					}
				}
			} else if arrayLen == 3 {
				if k == 0 {
					if column, ok := i.(string); ok {
						filters.Column = column
						filters.Single = true
						if !validColumn(column, config) {
							filters.Invalid = true
						}
					} else {
						filters.Invalid = true // N-06: non-string column
					}
				} else if k == 1 {
					if operator, ok := i.(string); ok {
						// Strict comparison-operator allowlist (F-01).
						op, valid := normalizeOperator(operator, config)
						filters.Operator = op
						filters.Single = true
						if !valid {
							filters.Invalid = true
						}
					} else {
						filters.Invalid = true // N-06: non-string operator
					}
				} else if k == 2 {
					if filters.Column == "" && len(subFilters) > 0 {
						stray = true // V3-01: value alongside group members
					}
					if strings.Contains(filters.Column, ",") {
						subFilters = filterToSubFilter(&filters, i, config, depth)
						continue
					}
					switch filters.Operator {
					case "IN", "NOT IN":
						filters.Value = i
						values, ok := i.([]interface{})
						if !ok || len(values) > maxListValues {
							filters.Invalid = true // C-01: list length cap
						}
						if filters.Operator == "NOT IN" {
							// C-09: a NULL member makes NOT IN unknown for every row.
							for _, v := range values {
								if v == nil {
									filters.Invalid = true
									break
								}
							}
						}
					case "BETWEEN":
						filters.Value = i
						if values, ok := i.([]interface{}); !ok || len(values) < 2 {
							filters.Invalid = true
						}
					case "LIKE", "ILIKE", "NOT LIKE", "NOT ILIKE":
						// V3-05: escape unconditionally — including when no
						// Statement is present (direct/programmatic callers).
						// C-12f: "!" needs no string-literal escaping on any dialect.
						filters.ValueSuffix = "ESCAPE '" + likeEscapeChar + "'"
						value, ok := likeValueString(i)
						if !ok {
							filters.Invalid = true // C-12a: arrays/objects/null
						}
						value = escapeLikeValue(value, likeEscapeChar)
						if config.SmartSearchEnabled {
							value = smartSearchRe.ReplaceAllString(value, "%")
						}
						filters.Value = "%" + value + "%"
					default:
						filters.Value = i
					}
				}
			} else if arrayLen > 3 {
				// N-06: elements outside documented 1/2/3-tuples are garbage.
				// All-array groups take the subFilters branch above and never
				// reach here, so only ambiguous shapes are rejected.
				filters.Invalid = true
			}
		}
		// P-01b: fast path — reuse the slice when no separator insertion and
		// no trailing connector require a rebuild.
		if len(subFilters) > 0 {
			// N-11/V3-01: a stray element (incl. nil, which the old `Value != nil`
			// check missed) occupying the Value slot while group members exist is
			// about to be overwritten — flag it in strict mode.
			if config.StrictFilters && stray {
				filters.Invalid = true
			}
			if subFilters[0].IsOperator {
				filters.Invalid = true // N-06: group cannot start with a connector
			}
			for k := 1; k < len(subFilters); k++ {
				if subFilters[k-1].IsOperator && subFilters[k].IsOperator {
					filters.Invalid = true // C-08: "OR OR" is never valid SQL
					break
				}
			}
			for _, s := range subFilters {
				if !s.IsOperator && !s.Single && s.Column == "" && s.Value == nil && !s.Invalid {
					// V3-01: an empty-array member (e.g. filter "[[]]") is
					// garbage in any mode — reject instead of emitting
					// "WHERE ( ( )" (and instead of strict-only detection).
					filters.Invalid = true
					break
				}
			}
			for len(subFilters) > 0 && subFilters[len(subFilters)-1].IsOperator {
				subFilters = subFilters[:len(subFilters)-1]
			}
			needsSep := false
			for k := 1; k < len(subFilters); k++ {
				if !subFilters[k-1].IsOperator && !subFilters[k].IsOperator {
					needsSep = true
					break
				}
			}
			if !needsSep {
				filters.Value = subFilters
			} else {
				separatedSubFilters := []pageFilters{}
				hasOperator := false
				for _, s := range subFilters {
					if !hasOperator && !s.IsOperator && len(separatedSubFilters) > 0 {
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
			}
			filters.Single = false
			filters.IsOperator = false
		}
	}

	return filters
}

func filterToSubFilter(filters *pageFilters, value interface{}, config Config, depth int) []pageFilters {
	subFilters := []pageFilters{}
	// Reject malformed multi-column references instead of silently mangling them.
	if !validColumn(filters.Column, config) {
		filters.Invalid = true
		return subFilters
	}
	columns := strings.Split(filters.Column, ",")
	if len(columns) > maxFilterColumns {
		filters.Invalid = true // C-01: columns × values amplification cap
		return subFilters
	}
	columnRepeat := []interface{}{}
	for _, col := range columns {
		columnRepeat = append(columnRepeat, []interface{}{col, filters.Operator, value})
	}

	filters.Column = ""
	filters.Single = false
	filters.Operator = ""
	filters.IsOperator = false
	subFilters = append(subFilters, arrayToFilterDepth(columnRepeat, config, depth+1))

	return subFilters
}

// generateWhereCauses builds the WHERE fragments for a filter tree. The root
// call treats a zero node as "no filters" (empty output); every nested call
// fails closed instead (see generateWhereCausesNested).
func generateWhereCauses(f pageFilters, config Config) ([]string, []interface{}) {
	return generateWhereCausesNested(f, config, false)
}

//gocyclo:ignore
func generateWhereCausesNested(f pageFilters, config Config, nested bool) ([]string, []interface{}) {
	wheres := []string{}
	params := []interface{}{}

	// Fail closed on any rejected operator/column/shape (F-01/F-02/F-07).
	if f.Invalid {
		wheres = append(wheres, "1 = 0")
		return wheres, params
	}

	if !f.Single && !f.IsOperator {
		// V3-01 (AGY3-01): a zero node inside a tree is a garbage group
		// member (e.g. filter "[[]]") — never emit "WHERE ( ( ) )".
		if f.Value == nil && f.Column == "" && nested {
			wheres = append(wheres, "1 = 0")
			return wheres, params
		}
		ifaces, ok := f.Value.([]pageFilters)
		if ok && len(ifaces) > 0 {
			// V4-03: a zero or Invalid member fails the whole group closed as
			// a SINGLE fragment — never emit connector-less adjacent fragments
			// like "1 = 0 name = ?" from hand-built trees.
			for _, i := range ifaces {
				if i.Invalid || (!i.IsOperator && !i.Single && i.Column == "" && i.Value == nil) {
					return []string{"1 = 0"}, nil
				}
			}
			wheres = append(wheres, "(")
			hasOpen := false
			for _, i := range ifaces {
				subs, isSub := i.Value.([]pageFilters)
				regular, isNotSub := i.Value.(pageFilters)
				if isSub && len(subs) > 0 {
					wheres = append(wheres, "(")
					for _, s := range subs {
						subWheres, subParams := generateWhereCausesNested(s, config, true)
						wheres = append(wheres, subWheres...)
						params = append(params, subParams...)
					}
					wheres = append(wheres, ")")
				} else if isNotSub {
					subWheres, subParams := generateWhereCausesNested(regular, config, true)
					wheres = append(wheres, subWheres...)
					params = append(params, subParams...)
				} else {
					if !hasOpen && !i.IsOperator {
						wheres = append(wheres, "(")
						hasOpen = true
					}
					subWheres, subParams := generateWhereCausesNested(i, config, true)
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
			// Connectors only — anything else is rejected (F-01).
			if isConnector(f.Operator) {
				wheres = append(wheres, f.Operator)
			} else {
				wheres = append(wheres, "1 = 0")
			}
		} else {
			fname := fieldName(f.Column)
			dialect := ""
			if nil != config.Statement {
				fname = config.Statement.Quote(fname)
				if config.Statement.Dialector != nil {
					dialect = config.Statement.Dialector.Name()
				}
			}
			switch f.Operator {
			case "IS", "IS NOT":
				strValue, isStr := f.Value.(string)
				boolValue, isBool := f.Value.(bool)
				switch {
				case nil == f.Value || (isStr && strings.ToLower(strValue) == "null"):
					wheres = append(wheres, fname, f.Operator, "NULL")
				case isBool && (dialect == "postgres" || dialect == "mysql"):
					wheres = append(wheres, fname, f.Operator, strings.ToUpper(strconv.FormatBool(boolValue)))
				case dialect == "postgres":
					// C-12b: "IS $1" is a syntax error; IS [NOT] DISTINCT FROM
					// is the null-safe comparison sqlite's IS performs.
					op := "IS NOT DISTINCT FROM"
					if f.Operator == "IS NOT" {
						op = "IS DISTINCT FROM"
					}
					wheres = append(wheres, fname, op, "?")
					params = append(params, valueFixer(f.Value))
				case dialect == "mysql":
					if f.Operator == "IS NOT" {
						wheres = append(wheres, "NOT", "(", fname, "<=>", "?", ")")
					} else {
						wheres = append(wheres, fname, "<=>", "?")
					}
					params = append(params, valueFixer(f.Value))
				default:
					wheres = append(wheres, fname, f.Operator, "?")
					params = append(params, valueFixer(f.Value))
				}
			case "BETWEEN":
				if values, ok := f.Value.([]interface{}); ok && len(values) >= 2 {
					wheres = append(wheres, "(", fname, f.Operator, "? AND ?", ")")
					params = append(params, valueFixer(values[0]), valueFixer(values[1]))
				} else {
					wheres = append(wheres, "1 = 0")
				}
			case "IN", "NOT IN":
				if values, ok := f.Value.([]interface{}); ok && len(values) == 0 {
					// C-09: gorm renders [] as (NULL), and NOT IN (NULL) matches
					// nothing; excluding an empty set must keep every row.
					if f.Operator == "NOT IN" {
						wheres = append(wheres, "1 = 1")
					} else {
						wheres = append(wheres, "1 = 0")
					}
				} else if ok {
					wheres = append(wheres, fname, f.Operator, "?")
					params = append(params, valueFixer(values))
				} else {
					wheres = append(wheres, "1 = 0")
				}
			case "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE":
				if config.FieldWrapper != "" {
					fname = fmt.Sprintf(config.FieldWrapper, fname)
				}
				operator := f.Operator
				if dialect != "" && dialect != "postgres" {
					// C-12c: ILIKE is postgres-only; the default LOWER()
					// wrappers already make LIKE case-insensitive elsewhere.
					operator = strings.Replace(operator, "ILIKE", "LIKE", 1)
				}
				wheres = append(wheres, fname, operator, "?")
				if f.ValueSuffix != "" {
					wheres = append(wheres, f.ValueSuffix)
				}
				value, isStrValue := f.Value.(string)
				if isStrValue {
					if config.ValueWrapper != "" {
						value = fmt.Sprintf(config.ValueWrapper, value)
					} else if !config.LikeAsIlikeDisabled {
						value = strings.ToLower(value)
					}
					params = append(params, value)
				} else {
					params = append(params, f.Value)
				}
			default:
				// N-06 defense-in-depth: never interpolate an operator that is
				// not in the allowlist (parse-time validation should have
				// rejected it already; this covers programmatic construction).
				if !isAllowedOperator(f.Operator, config) {
					wheres = append(wheres, "1 = 0")
				} else {
					wheres = append(wheres, fname, f.Operator, "?")
					params = append(params, valueFixer(f.Value))
				}
			}
		}
	}

	return wheres, params
}

// likeValueString renders a scalar LIKE operand (C-12a): numbers keep their
// literal form instead of %v's exponent notation; arrays, objects and null
// are not searchable text and are rejected.
func likeValueString(v interface{}) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case nil, []interface{}, map[string]interface{}:
		return "", false
	default:
		return fmt.Sprintf("%v", t), true
	}
}

func valueFixer(n interface{}) interface{} {
	var values []interface{}
	if rawValues, ok := n.([]interface{}); ok {
		for i := range rawValues {
			values = append(values, valueFixer(rawValues[i]))
		}

		return values
	}
	// C-06: json.Number (default request decoding) keeps full integer
	// precision; whole floats within the exact range bind as int64 (the old
	// FormatFloat('g') check silently skipped every value >= 1e6).
	switch v := n.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil {
			return f
		}
		return v.String()
	case float64:
		if v == math.Trunc(v) && math.Abs(v) <= 1<<53 {
			return int64(v)
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
	// P-03: single pass — no Split/append/Join; only the dotted case builds
	// a new string (and strcase.ToCamel allocates regardless).
	idx := strings.IndexByte(field, '.')
	if idx < 0 {
		return field
	}
	return strcase.ToCamel(field[:idx]) + "__" + strings.ReplaceAll(field[idx+1:], ".", "__")
}

// Config for customize pagination result
type Config struct {
	Operator             string
	FieldWrapper         string
	ValueWrapper         string
	DefaultSize          int64
	PageStart            int64
	LikeAsIlikeDisabled  bool
	SmartSearchEnabled   bool
	Statement            *gorm.Statement `json:"-"`
	CustomParamEnabled   bool
	SortParams           []string
	PageParams           []string
	OrderParams          []string
	SizeParams           []string
	FilterParams         []string
	FieldsParams         []string
	FieldSelectorEnabled bool
	CacheAdapter         gocache.AdapterInterface               `json:"-"`
	JSONMarshal          func(v interface{}) ([]byte, error)    `json:"-"`
	JSONUnmarshal        func(data []byte, v interface{}) error `json:"-"`
	ErrorEnabled         bool
	// MaxSize caps the client-provided page size. Zero means defaultMaxPageSize (100).
	MaxSize int64
	// ExtraOperators extends the operator allowlist (advanced, e.g. "SIMILAR TO").
	// Entries are sanitized at config load: upper-cased, and anything that is
	// not letters/digits/space is dropped — extras become SQL operator syntax
	// and must never contain metacharacters (N-08).
	ExtraOperators []string
	// ColumnValidator, when set, replaces the default identifier validation.
	ColumnValidator func(string) bool `json:"-"`
	// CacheScope, when set, is mixed into the cache key so different
	// users/tenants with identical parameters never share cached pages.
	// REQUIRED whenever CacheAdapter may serve more than one user/tenant.
	CacheScope func() string `json:"-"`
	// StrictFilters rejects filter trees containing nodes that produce no
	// WHERE fragments (e.g. dropped garbage elements) instead of ignoring
	// them (N-11). Default false preserves lenient v1.x behavior.
	StrictFilters bool
	// AllowHiddenColumns re-enables filtering, sorting and selecting on
	// destination fields tagged `json:"-"` (R-01). Default false rejects them:
	// a hidden column used in a filter leaks its value through `total`.
	AllowHiddenColumns bool
}

// pageFilters struct
type pageFilters struct {
	Column      string
	Operator    string
	Value       interface{}
	ValueSuffix string
	Single      bool
	IsOperator  bool
	Fields      []string
	// Invalid marks a node rejected by operator/column/shape validation;
	// the whole filter fails closed (1 = 0) when any node carries it.
	Invalid bool
}

// Page result wrapper
type Page struct {
	Items        interface{} `json:"items"`
	Page         int64       `json:"page"`
	Size         int64       `json:"size"`
	MaxPage      int64       `json:"max_page"`
	TotalPages   int64       `json:"total_pages"`
	Total        int64       `json:"total"`
	Last         bool        `json:"last"`
	First        bool        `json:"first"`
	Visible      int64       `json:"visible"`
	Error        bool        `json:"error,omitempty"`
	ErrorMessage string      `json:"error_message,omitempty"`
	RawError     error       `json:"-"`
}

// Request struct
type Request struct {
	Page    int64       `json:"page"`
	Size    int64       `json:"size"`
	Sort    string      `json:"sort"`
	Order   string      `json:"order"`
	Fields  []string    `json:"fields"`
	Filters interface{} `json:"filters"`
}

// query struct
type requestQuery struct {
	WhereString  string
	Wheres       []string
	Params       []interface{}
	Sorts        []sortOrder
	Limit        int64
	Offset       int64
	Invalid      bool
	ErrorMessage string
}

// pageRequest struct
type pageRequest struct {
	Size    int64
	Page    int64
	Sorts   []sortOrder
	Filters pageFilters
	Config  Config `json:"-"`
	Fields  []string
	// Invalid marks parse-level rejections (malformed/oversized bodies);
	// excluded from the cache-key JSON.
	Invalid      bool   `json:"-"`
	ErrorMessage string `json:"-"`
}

// sortOrder struct
type sortOrder struct {
	Column    string
	Direction string
}

func createCacheKey(cachePrefix string, pr pageRequest, scope string) string {
	key := ""
	if bte, err := pr.Config.JSONMarshal(pr); nil == err && cachePrefix != "" {
		if scope != "" {
			// Scope (tenant/user) is hashed into the key so callers with
			// identical parameters never share cached pages (F-06).
			// Length-prefixed (C-12h) so no scope/params split can collide.
			bte = append([]byte(strconv.Itoa(len(scope))+":"+scope+"|"), bte...)
		}
		// Keys are "prefix:hash". The ":" lets ClearCache scope clears
		// exactly (it appends ":"); note ClearPrefix("user") with a raw
		// adapter still matches "user1:..." — pass "user:" yourself there.
		key = fmt.Sprintf("%s:%x", cachePrefix, sha256.Sum256(bte))
	}

	return key
}

func defaultConfig(c *Config) *Config {
	if nil == c {
		return &Config{
			JSONMarshal:   json.Marshal,
			JSONUnmarshal: json.Unmarshal,
		}
	}

	if nil == c.JSONMarshal {
		c.JSONMarshal = json.Marshal
	}

	if nil == c.JSONUnmarshal {
		c.JSONUnmarshal = json.Unmarshal
	}

	c.ExtraOperators = sanitizeExtraOperators(c.ExtraOperators)

	return c
}

// sanitizeExtraOperators normalizes extras to upper-case and drops anything
// containing non [A-Z0-9 ] characters (N-08): extras are interpolated as SQL
// operator syntax and must never carry metacharacters.
func sanitizeExtraOperators(extras []string) []string {
	if len(extras) == 0 {
		return extras
	}
	cleaned := make([]string, 0, len(extras))
	for _, e := range extras {
		op := strings.ToUpper(strings.TrimSpace(e))
		if validExtraOperator(op) && !contains(cleaned, op) {
			cleaned = append(cleaned, op)
		} else {
			extraOpWarnOnce.Do(func() {
				log.Printf("paginate: dropping invalid ExtraOperators entry %q (letters/digits/space only)", e)
			})
		}
	}
	return cleaned
}

func validExtraOperator(op string) bool {
	if op == "" {
		return false
	}
	hasLetter := false
	for i := 0; i < len(op); i++ {
		ch := op[i]
		switch {
		case ch >= 'A' && ch <= 'Z':
			hasLetter = true
		case ch >= '0' && ch <= '9', ch == ' ':
		default:
			return false
		}
	}
	if !hasLetter {
		return false
	}
	// V4-02: extras are comparison operators, never logical connectors —
	// entries like "OR 1" or "IS NOT NULL OR TRUE" must not survive (C-12g:
	// nor MySQL's XOR / arithmetic DIV / MOD).
	for _, tok := range strings.Fields(op) {
		if tok == "OR" || tok == "AND" || tok == "XOR" || tok == "DIV" || tok == "MOD" {
			return false
		}
	}
	return true
}
