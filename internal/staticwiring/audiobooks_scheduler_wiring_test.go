// Package staticwiring contains standalone, dependency-free static-asset
// assertions for the audiobook-scheduler slice. It exists as its own
// directory/package (test files only - no production code) specifically so
// these checks can run without pulling in the rest of the module's build
// graph: package main and internal/monitor/... currently fail to even
// compile on a Windows/CGO-disabled host (pre-existing, unrelated to this
// slice - go-fuse and several internal packages use Unix-only syscalls
// without the right build constraints), which would make any test placed
// there report a compile failure instead of a behavioral one on this host.
package staticwiring

// Requirement A7/E5 (see review-1.md): the native scheduler config, main.go
// job map, the dashboard log-file allowlist, the dashboard order/log
// selector, and the settings save/load mapping must all genuinely include
// "audiobooks" - not merely contain the string "audiobooks" somewhere in a
// comment or an unrelated literal (review-1.md's exact counterexample: "an
// unrelated string audiobooks in each searched block satisfy all current
// static tests").
//
// Go source files (main.go, handler.go) are checked with go/parser's AST,
// not text search: each assertion locates the specific named composite
// literal (the "syncers" map, the "schedCfg" SchedulerConfig literal, the
// "allowed" log-file map) and inspects its actual keys/fields. Comments are
// not part of the AST, so a comment mentioning "audiobooks" cannot satisfy
// these checks, and a string literal with the right *value* in the wrong
// *literal* (e.g. an unrelated map elsewhere in the file) cannot either.
//
// dashboard.html and settings.html are not Go source, so they are checked
// with comment-stripped, tightly-scoped regexes: JS `//` line comments and
// HTML `<!-- -->` block comments are stripped first (defeating "the
// audiobooks-sync case is only mentioned in a comment"), then each
// assertion is scoped to the exact object/array literal it must appear in
// (the job order array, the log <select>, the jobMap toggle-button map, the
// loadScheduler field reads, and the saveScheduler payload object) rather
// than searched for anywhere in the file.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// repoRoot walks up from this test file's own directory until it finds
// go.mod, so the test does not depend on the current working directory
// go test happens to use.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 32; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (go.mod) above " + thisFile)
	return ""
}

func readRepoFile(t *testing.T, relPath string) string {
	t.Helper()
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return string(data)
}

// sliceBetween returns the substring of s starting at the first occurrence
// of start and ending at the first occurrence of end after it (exclusive of
// end), or fails the test if either marker is missing. Scoping the search
// this way avoids false positives from an unrelated mention of "audiobook"
// elsewhere in a large shared file.
func sliceBetween(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("marker %q not found after %q", end, start)
	}
	return rest[:j]
}

// stripJSLineComments removes `// ...` line comments, using the same
// "don't strip after a colon" heuristic internal/config/config.go's own
// stripJSONComments uses, so an "http://" URL survives.
func stripJSLineComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		idx := strings.Index(line, "//")
		for idx > 0 && line[idx-1] == ':' {
			next := strings.Index(line[idx+2:], "//")
			if next < 0 {
				idx = -1
				break
			}
			idx = idx + 2 + next
		}
		if idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

var htmlCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)

func stripHTMLComments(s string) string {
	return htmlCommentPattern.ReplaceAllString(s, "")
}

// --- Go source (AST-based) assertions ---

func mustParseGoFile(t *testing.T, relPath string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), relPath), nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", relPath, err)
	}
	return file
}

// findAssignedCompositeLit locates `varName := <CompositeLit>` (or `varName
// = <CompositeLit>`) anywhere in the file's function bodies and returns the
// literal. It does not match names inside comments or string literals,
// since those are not part of the AST.
func findAssignedCompositeLit(t *testing.T, file *ast.File, varName string) *ast.CompositeLit {
	t.Helper()
	var found *ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok || ident.Name != varName || i >= len(assign.Rhs) {
				continue
			}
			if cl, ok := assign.Rhs[i].(*ast.CompositeLit); ok {
				found = cl
			}
		}
		return true
	})
	if found == nil {
		t.Fatalf("could not find a composite literal assigned to %q", varName)
	}
	return found
}

func compositeLitHasStringKey(cl *ast.CompositeLit, key string) bool {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		lit, ok := kv.Key.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if unquoted, err := strconv.Unquote(lit.Value); err == nil && unquoted == key {
			return true
		}
	}
	return false
}

func compositeLitHasFieldKey(cl *ast.CompositeLit, field string) bool {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == field {
			return true
		}
	}
	return false
}

// A7: main.go's scheduler syncers map literal (the same one that builds
// movies/tv/music/watchlist and feeds scheduler.New) must have a real
// "audiobooks" key - checked via the map literal's actual AST keys, so a
// comment or an unrelated string elsewhere in main.go cannot satisfy this.
func TestMainGo_SchedulerSyncersMap_RegistersAudiobooksJob(t *testing.T) {
	cl := findAssignedCompositeLit(t, mustParseGoFile(t, "main.go"), "syncers")
	if !compositeLitHasStringKey(cl, "audiobooks") {
		t.Fatal(`main.go's "syncers" map literal has no "audiobooks" key`)
	}
}

// A7: the native SchedulerConfig literal ("schedCfg", the same value passed
// to scheduler.New) must carry an AudiobooksSync field alongside
// MoviesSync/TVSync/MusicSync/WatchlistSync, checked via the literal's
// actual AST field keys.
func TestMainGo_SchedulerConfigLiteral_CarriesAudiobooksSchedule(t *testing.T) {
	cl := findAssignedCompositeLit(t, mustParseGoFile(t, "main.go"), "schedCfg")
	if !compositeLitHasFieldKey(cl, "AudiobooksSync") {
		t.Fatal(`main.go's "schedCfg" SchedulerConfig literal has no AudiobooksSync field`)
	}
}

// A7: the dashboard's server-side log-file allowlist (handler.go's Logs
// endpoint) must permit "audiobooks-sync" the same way it already permits
// movies-sync/tv-sync/watchlist-sync/music-sync - checked via the map
// literal's actual AST keys, not a text search, so the handler cannot
// silently keep rejecting the audiobooks log while some unrelated part of
// the file happens to mention the string.
func TestDashboardHandler_LogAllowlist_IncludesAudiobooksSync(t *testing.T) {
	cl := findAssignedCompositeLit(t, mustParseGoFile(t, "internal/monitor/dashboard/handler.go"), "allowed")
	if !compositeLitHasStringKey(cl, "audiobooks-sync") {
		t.Fatal(`handler.go's "allowed" log-file map has no "audiobooks-sync" key`)
	}
}

// --- dashboard.html / settings.html (comment-stripped, scoped) assertions ---

// A7: the monitor dashboard's job status order and its log file selector
// must include the audiobooks job, following the existing "<job>-sync" log
// selector convention (movies-sync, tv-sync, watchlist-sync, music-sync).
// Comments are stripped first so a commented-out mention cannot pass.
func TestDashboardHTML_IncludesAudiobooksJobAndLogSelector(t *testing.T) {
	src := stripHTMLComments(readRepoFile(t, "internal/monitor/dashboard/dashboard.html"))

	orderBlock := sliceBetween(t, src, "const order=[", "];")
	if !regexp.MustCompile(`'audiobooks'`).MatchString(orderBlock) {
		t.Fatalf("dashboard.html scheduler order array does not include 'audiobooks': %s", orderBlock)
	}

	logSelectBlock := sliceBetween(t, src, `id="logFile"`, "</select>")
	if !regexp.MustCompile(`<option\s+value="audiobooks-sync"`).MatchString(logSelectBlock) {
		t.Fatalf("dashboard.html log selector does not offer an <option value=\"audiobooks-sync\">:\n%s", logSelectBlock)
	}
}

// A7: the Control Panel must expose a real audiobooks schedule control
// (some element whose id starts with "sched-audiobooks", inside the actual
// scheduler card markup - not merely a passing mention anywhere on the
// page), and its run/stop button-state toggle map (jobMap) must include the
// audiobooks job the same way it already includes movies/tv/watchlist.
func TestSettingsHTML_SchedulerControl_HasRealAudiobooksElementAndToggleMapping(t *testing.T) {
	src := stripHTMLComments(stripJSLineComments(readRepoFile(t, "settings.html")))

	card := sliceBetween(t, src, `id="sched-body"`, `id="sched-save-status"`)
	if !regexp.MustCompile(`id="sched-audiobooks`).MatchString(card) {
		t.Fatalf(`settings.html scheduler card has no element with an id starting "sched-audiobooks":\n%s`, card)
	}

	jobMapBlock := sliceBetween(t, src, "const jobMap = {", "};")
	if !regexp.MustCompile(`:\s*'audiobooks'`).MatchString(jobMapBlock) {
		t.Fatalf(`settings.html's jobMap does not map any key to 'audiobooks': %s`, jobMapBlock)
	}
}

// A7/E5: loadScheduler must read an audiobooks_sync field back into the
// audiobooks control, and saveScheduler's payload object must both send an
// audiobooks_sync field and continue preserving music_sync (which, per the
// existing comment in this exact function, has no dedicated UI row and must
// be read back from the current config rather than silently dropped). Both
// checks are scoped to the exact object literal that matters, so discarding
// the schedule or losing music_sync while merely mentioning "audiobooks"
// elsewhere cannot pass.
func TestSettingsHTML_LoadAndSaveScheduler_MapAudiobooksScheduleAndPreserveMusicSync(t *testing.T) {
	src := stripHTMLComments(stripJSLineComments(readRepoFile(t, "settings.html")))

	loadFn := sliceBetween(t, src, "async function loadScheduler()", "async function saveScheduler")
	if !regexp.MustCompile(`sched\.audiobooks_sync`).MatchString(loadFn) {
		t.Fatalf("settings.html's loadScheduler does not read sched.audiobooks_sync:\n%s", loadFn)
	}

	saveFn := sliceBetween(t, src, "async function saveScheduler()", "async function runSyncNow")
	payload := sliceBetween(t, saveFn, "data.scheduler = {", "};")
	if !regexp.MustCompile(`\baudiobooks_sync\s*:`).MatchString(payload) {
		t.Fatalf("settings.html's saveScheduler payload does not include an audiobooks_sync field:\n%s", payload)
	}
	if !regexp.MustCompile(`\bmusic_sync\s*:`).MatchString(payload) {
		t.Fatalf("settings.html's saveScheduler payload no longer preserves music_sync:\n%s", payload)
	}
}

// A7/E5 (review-2.md): the scheduler settings block (the scheduler card's
// HTML plus its load/save/run/stop JS - the same scope the two tests above
// already isolate) must never read, write, insert into the DOM, or log an
// audiobook/Prowlarr secret or source-material field. This is scoped to
// exactly that block, not the whole file, because settings.html legitimately
// has an unrelated Prowlarr API key input elsewhere on the page for general
// Prowlarr configuration - this test must not false-positive on that.
func TestSettingsHTML_SchedulerBlock_NeverReferencesAudiobookOrProwlarrSecretFields(t *testing.T) {
	src := stripHTMLComments(stripJSLineComments(readRepoFile(t, "settings.html")))

	card := sliceBetween(t, src, `id="sched-body"`, `id="sched-save-status"`)
	loadFn := sliceBetween(t, src, "async function loadScheduler()", "async function saveScheduler")
	saveFn := sliceBetween(t, src, "async function saveScheduler()", "async function runSyncNow")
	runStopFns := sliceBetween(t, src, "async function runSyncNow(type)", "</script>")

	schedulerBlock := card + "\n" + loadFn + "\n" + saveFn + "\n" + runStopFns

	forbidden := []string{
		"audiosilo_token",
		"audiosilo_url",
		"audiobookshelf_token",
		"api_key",
		"magnet:",
		"download_url",
		"downloadUrl",
		"torrent_file",
	}
	for _, term := range forbidden {
		if regexp.MustCompile(`(?i)` + regexp.QuoteMeta(term)).MatchString(schedulerBlock) {
			t.Fatalf("settings.html's scheduler block references forbidden secret/source-material term %q:\n%s", term, schedulerBlock)
		}
	}
}

// exprSource renders an AST expression back to source text, so this file
// can assert on an expression's actual shape (e.g. "gc().Scheduler.AudiobooksSync")
// without manually walking every SelectorExpr/CallExpr node, and without
// caring whether it is wrapped in a same-shape type conversion.
func exprSource(t *testing.T, fset *token.FileSet, expr ast.Expr) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		t.Fatalf("render expression: %v", err)
	}
	return buf.String()
}

func compositeLitValueFor(cl *ast.CompositeLit, key string) (ast.Expr, bool) {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// A7 (review-2.md): schedCfg's AudiobooksSync field must actually be sourced
// from gc().Scheduler.AudiobooksSync - not an empty/zero-value literal like
// `AudiobooksSync: scheduler.DailyJobConfig{}` (review's exact
// counterexample). Checked by rendering the field's value expression back to
// source and requiring it to mention that exact selector chain, so any
// wrapping type conversion (matching the existing MoviesSync/TVSync/MusicSync
// pattern) still passes.
func TestMainGo_SchedCfgAudiobooksSync_SourcedFromConfiguredSchedule(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), "main.go"), nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	cl := findAssignedCompositeLit(t, file, "schedCfg")
	value, ok := compositeLitValueFor(cl, "AudiobooksSync")
	if !ok {
		t.Fatal(`main.go's "schedCfg" literal has no AudiobooksSync field`)
	}
	rendered := exprSource(t, fset, value)
	if !strings.Contains(rendered, "gc().Scheduler.AudiobooksSync") {
		t.Fatalf(`main.go's schedCfg.AudiobooksSync = %q, want it sourced from gc().Scheduler.AudiobooksSync`, rendered)
	}
}

// A7/A8 (review-2.md): the syncers map's "audiobooks" entry must be a real
// *audiobookjob.Syncer built from the configured audiobook fields - not an
// unrelated fake or an empty struct (review's exact counterexample: "map
// 'audiobooks' to an unrelated fake"). Checked structurally (it must be
// &audiobookjob.Syncer{...}) and by requiring its rendered source to
// reference both gc().Audiobooks (the audiobook-specific settings) and
// gc().Prowlarr (the shared Prowlarr settings the brief requires this job
// to narrow), so an empty or single-field Cfg cannot pass.
func TestMainGo_SyncersMapAudiobooksEntry_IsRealSyncerBuiltFromConfiguredFields(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), "main.go"), nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	cl := findAssignedCompositeLit(t, file, "syncers")

	var value ast.Expr
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		lit, ok := kv.Key.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if unquoted, err := strconv.Unquote(lit.Value); err == nil && unquoted == "audiobooks" {
			value = kv.Value
		}
	}
	if value == nil {
		t.Fatal(`main.go's "syncers" map literal has no "audiobooks" key`)
	}

	unary, ok := value.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		t.Fatalf(`main.go's syncers["audiobooks"] value is not a &audiobookjob.Syncer{...} literal: %s`, exprSource(t, fset, value))
	}
	inner, ok := unary.X.(*ast.CompositeLit)
	if !ok {
		t.Fatalf(`main.go's syncers["audiobooks"] value is not "&<CompositeLit>": %s`, exprSource(t, fset, value))
	}
	typeName := exprSource(t, fset, inner.Type)
	if typeName != "audiobookjob.Syncer" {
		t.Fatalf(`main.go's syncers["audiobooks"] value has type %q, want "audiobookjob.Syncer"`, typeName)
	}

	rendered := exprSource(t, fset, inner)
	if !strings.Contains(rendered, "gc().Audiobooks") {
		t.Fatalf("main.go's audiobooks Syncer literal does not reference gc().Audiobooks:\n%s", rendered)
	}
	if !strings.Contains(rendered, "gc().Prowlarr") {
		t.Fatalf("main.go's audiobooks Syncer literal does not reference gc().Prowlarr:\n%s", rendered)
	}
}
