// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// TODO(andybons): add logging
// TODO(andybons): restrict memory use

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/bradfitz/gomemcache/memcache"
	"golang.org/x/playground/internal/gcpdial"
	"golang.org/x/playground/sandbox/sandboxtypes"
)

const (
	maxCompileTime = 5 * time.Second
	maxRunTime     = 5 * time.Second

	// progName is the implicit program name written to the temp
	// dir and used in compiler and vet errors.
	progName = "prog.go"
)

const goBuildTimeoutError = "timeout running go build"

// Responses that contain these strings will not be cached due to
// their non-deterministic nature.
var nonCachingErrors = []string{
	"out of memory",
	"cannot allocate memory",
	goBuildTimeoutError,
}

type request struct {
	Body    string
	WithVet bool // whether client supports vet response in a /compile request (Issue 31970)
}

type response struct {
	Errors      string
	Events      []Event
	Status      int
	IsTest      bool
	TestsFailed int

	// VetErrors, if non-empty, contains any vet errors. It is
	// only populated if request.WithVet was true.
	VetErrors string `json:",omitempty"`
	// VetOK reports whether vet ran & passsed. It is only
	// populated if request.WithVet was true. Only one of
	// VetErrors or VetOK can be non-zero.
	VetOK bool `json:",omitempty"`
}

// commandHandler returns an http.HandlerFunc.
// This handler creates a *request, assigning the "Body" field a value
// from the "body" form parameter or from the HTTP request body.
// If there is no cached *response for the combination of cachePrefix and request.Body,
// handler calls cmdFunc and in case of a nil error, stores the value of *response in the cache.
// The handler returned supports Cross-Origin Resource Sharing (CORS) from any domain.
func (s *server) commandHandler(cachePrefix string, cmdFunc func(context.Context, *request) (*response, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cachePrefix := cachePrefix // so we can modify it below
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == "OPTIONS" {
			// This is likely a pre-flight CORS request.
			return
		}

		var req request
		// Until programs that depend on golang.org/x/tools/godoc/static/playground.js
		// are updated to always send JSON, this check is in place.
		if b := r.FormValue("body"); b != "" {
			req.Body = b
			req.WithVet, _ = strconv.ParseBool(r.FormValue("withVet"))
		} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.log.Errorf("error decoding request: %v", err)
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}

		if req.WithVet {
			cachePrefix += "_vet" // "prog" -> "prog_vet"
		}

		resp := &response{}
		key := cacheKey(cachePrefix, req.Body)
		if err := s.cache.Get(key, resp); err != nil {
			if err != memcache.ErrCacheMiss {
				s.log.Errorf("s.cache.Get(%q, &response): %v", key, err)
			}
			resp, err = cmdFunc(r.Context(), &req)
			if err != nil {
				s.log.Errorf("cmdFunc error: %v", err)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			for _, e := range nonCachingErrors {
				if strings.Contains(resp.Errors, e) {
					s.log.Errorf("cmdFunc compilation error: %q", resp.Errors)
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
					return
				}
			}
			for _, el := range resp.Events {
				if el.Kind != "stderr" {
					continue
				}
				for _, e := range nonCachingErrors {
					if strings.Contains(el.Message, e) {
						s.log.Errorf("cmdFunc runtime error: %q", el.Message)
						http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
						return
					}
				}
			}
			if err := s.cache.Set(key, resp); err != nil {
				s.log.Errorf("cache.Set(%q, resp): %v", key, err)
			}
		}

		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(resp); err != nil {
			s.log.Errorf("error encoding response: %v", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(w, &buf); err != nil {
			s.log.Errorf("io.Copy(w, &buf): %v", err)
			return
		}
	}
}

func cacheKey(prefix, body string) string {
	h := sha256.New()
	io.WriteString(h, body)
	return fmt.Sprintf("%s-%s-%x", prefix, runtime.Version(), h.Sum(nil))
}

// isTestFunc tells whether fn has the type of a testing function.
func isTestFunc(fn *ast.FuncDecl) bool {
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 ||
		fn.Type.Params.List == nil ||
		len(fn.Type.Params.List) != 1 ||
		len(fn.Type.Params.List[0].Names) > 1 {
		return false
	}
	ptr, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	// We can't easily check that the type is *testing.T
	// because we don't know how testing has been imported,
	// but at least check that it's *T or *something.T.
	if name, ok := ptr.X.(*ast.Ident); ok && name.Name == "T" {
		return true
	}
	if sel, ok := ptr.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "T" {
		return true
	}
	return false
}

// isTest tells whether name looks like a test (or benchmark, according to prefix).
// It is a Test (say) if there is a character after Test that is not a lower-case letter.
// We don't want TesticularCancer.
func isTest(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) { // "Test" is ok
		return true
	}
	return ast.IsExported(name[len(prefix):])
}

// getTestProg returns source code that executes all valid tests and examples in src.
// If the main function is present or there are no tests or examples, it returns nil.
// getTestProg emulates the "go test" command as closely as possible.
// Benchmarks are not supported because of sandboxing.
func getTestProg(src []byte) []byte {
	fset := token.NewFileSet()
	// Early bail for most cases.
	f, err := parser.ParseFile(fset, progName, src, parser.ImportsOnly)
	if err != nil || f.Name.Name != "main" {
		return nil
	}

	// importPos stores the position to inject the "testing" import declaration, if needed.
	importPos := fset.Position(f.Name.End()).Offset

	var testingImported bool
	for _, s := range f.Imports {
		if s.Path.Value == `"testing"` && s.Name == nil {
			testingImported = true
			break
		}
	}

	// Parse everything and extract test names.
	f, err = parser.ParseFile(fset, progName, src, parser.ParseComments)
	if err != nil {
		return nil
	}

	var tests []string
	for _, d := range f.Decls {
		n, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := n.Name.Name
		switch {
		case name == "main":
			// main declared as a method will not obstruct creation of our main function.
			if n.Recv == nil {
				return nil
			}
		case isTest(name, "Test") && isTestFunc(n):
			tests = append(tests, name)
		}
	}

	// Tests imply imported "testing" package in the code.
	// If there is no import, bail to let the compiler produce an error.
	if !testingImported && len(tests) > 0 {
		return nil
	}

	// We emulate "go test". An example with no "Output" comment is compiled,
	// but not executed. An example with no text after "Output:" is compiled,
	// executed, and expected to produce no output.
	var ex []*doc.Example
	// exNoOutput indicates whether an example with no output is found.
	// We need to compile the program containing such an example even if there are no
	// other tests or examples.
	exNoOutput := false
	for _, e := range doc.Examples(f) {
		if e.Output != "" || e.EmptyOutput {
			ex = append(ex, e)
		}
		if e.Output == "" && !e.EmptyOutput {
			exNoOutput = true
		}
	}

	if len(tests) == 0 && len(ex) == 0 && !exNoOutput {
		return nil
	}

	if !testingImported && (len(ex) > 0 || exNoOutput) {
		// In case of the program with examples and no "testing" package imported,
		// add import after "package main" without modifying line numbers.
		importDecl := []byte(`;import "testing";`)
		src = bytes.Join([][]byte{src[:importPos], importDecl, src[importPos:]}, nil)
	}

	data := struct {
		Tests    []string
		Examples []*doc.Example
	}{
		tests,
		ex,
	}
	code := new(bytes.Buffer)
	if err := testTmpl.Execute(code, data); err != nil {
		panic(err)
	}
	src = append(src, code.Bytes()...)
	return src
}

var testTmpl = template.Must(template.New("main").Parse(`
func main() {
	matchAll := func(t string, pat string) (bool, error) { return true, nil }
	tests := []testing.InternalTest{
{{range .Tests}}
		{"{{.}}", {{.}}},
{{end}}
	}
	examples := []testing.InternalExample{
{{range .Examples}}
		{"Example{{.Name}}", Example{{.Name}}, {{printf "%q" .Output}}, {{.Unordered}}},
{{end}}
	}
	testing.Main(matchAll, tests, nil, examples)
}
`))

var failedTestPattern = "--- FAIL"

// compileAndRun tries to build and run a user program.
// The output of successfully ran program is returned in *response.Events.
// If a program cannot be built or has timed out,
// *response.Errors contains an explanation for a user.
func compileAndRun(ctx context.Context, req *request) (*response, error) {
	// TODO(andybons): Add semaphore to limit number of running programs at once.
	tmpDir, err := ioutil.TempDir("", "sandbox")
	if err != nil {
		return nil, fmt.Errorf("error creating temp directory: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	files, err := splitFiles([]byte(req.Body))
	if err != nil {
		return &response{Errors: err.Error()}, nil
	}

	var testParam string
	var buildPkgArg = "."
	if files.Num() == 1 && len(files.Data(progName)) > 0 {
		buildPkgArg = progName
		src := files.Data(progName)
		if code := getTestProg(src); code != nil {
			testParam = "-test.v"
			files.AddFile(progName, code)
		}
	}

	useModules := allowModuleDownloads(files)
	if !files.Contains("go.mod") && useModules {
		files.AddFile("go.mod", []byte("module play\n"))
	}

	for f, src := range files.m {
		// Before multi-file support we required that the
		// program be in package main, so continue to do that
		// for now. But permit anything in subdirectories to have other
		// packages.
		if !strings.Contains(f, "/") {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, f, src, parser.PackageClauseOnly)
			if err == nil && f.Name.Name != "main" {
				return &response{Errors: "package name must be main"}, nil
			}
		}

		in := filepath.Join(tmpDir, f)
		if strings.Contains(f, "/") {
			if err := os.MkdirAll(filepath.Dir(in), 0755); err != nil {
				return nil, err
			}
		}
		if err := ioutil.WriteFile(in, src, 0644); err != nil {
			return nil, fmt.Errorf("error creating temp file %q: %v", in, err)
		}
	}

	// TODO: simplify this once Go 1.14 is out. We should remove
	// the //play:gvisor substring hack and DEBUG_FORCE_GVISOR and
	// instead implement https://golang.org/issue/33629 to
	// officially support different Go versions (Go tip + past two
	// releases).
	useGvisor := os.Getenv("GO_VERSION") >= "go1.14" ||
		os.Getenv("DEBUG_FORCE_GVISOR") == "1" ||
		strings.Contains(req.Body, "//play:gvisor\n")

	exe := filepath.Join(tmpDir, "a.out")
	goCache := filepath.Join(tmpDir, "gocache")

	buildCtx, cancel := context.WithTimeout(ctx, maxCompileTime)
	defer cancel()
	goBin := "go"
	if useGvisor {
		goBin = "/usr/local/go1.14/bin/go"
	}
	cmd := exec.CommandContext(buildCtx, goBin,
		"build",
		"-o", exe,
		"-tags=faketime", // required for Go 1.14+, no-op before
		buildPkgArg)
	cmd.Dir = tmpDir
	var goPath string
	if useGvisor {
		cmd.Env = []string{"GOOS=linux", "GOARCH=amd64", "GOROOT=/usr/local/go1.14"}
	} else {
		cmd.Env = []string{"GOOS=nacl", "GOARCH=amd64p32"}
	}
	cmd.Env = append(cmd.Env, "GOCACHE="+goCache)
	if useModules {
		// Create a GOPATH just for modules to be downloaded
		// into GOPATH/pkg/mod.
		goPath, err = ioutil.TempDir("", "gopath")
		if err != nil {
			return nil, fmt.Errorf("error creating temp directory: %v", err)
		}
		defer os.RemoveAll(goPath)
		cmd.Env = append(cmd.Env, "GO111MODULE=on", "GOPROXY="+playgroundGoproxy())
	} else {
		goPath = os.Getenv("GOPATH")                 // contains old code.google.com/p/go-tour, etc
		cmd.Env = append(cmd.Env, "GO111MODULE=off") // in case it becomes on by default later
	}
	cmd.Env = append(cmd.Env, "GOPATH="+goPath)
	t0 := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		if buildCtx.Err() == context.DeadlineExceeded {
			log.Printf("go build timed out after %v", time.Since(t0))
			return &response{Errors: goBuildTimeoutError}, nil
		}
		if _, ok := err.(*exec.ExitError); ok {
			// Return compile errors to the user.

			// Rewrite compiler errors to strip the tmpDir name.
			errs := strings.Replace(string(out), tmpDir+"/", "", -1)

			// "go build", invoked with a file name, puts this odd
			// message before any compile errors; strip it.
			errs = strings.Replace(errs, "# command-line-arguments\n", "", 1)

			return &response{Errors: errs}, nil
		}
		return nil, fmt.Errorf("error building go source: %v", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, maxRunTime)
	defer cancel()
	rec := new(Recorder)
	var exitCode int
	if useGvisor {
		const maxBinarySize = 100 << 20 // copied from sandbox backend; TODO: unify?
		if fi, err := os.Stat(exe); err != nil || fi.Size() == 0 || fi.Size() > maxBinarySize {
			if err != nil {
				return nil, fmt.Errorf("failed to stat binary: %v", err)
			}
			return nil, fmt.Errorf("invalid binary size %d", fi.Size())
		}
		exeBytes, err := ioutil.ReadFile(exe)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(runCtx, "POST", sandboxBackendURL(), bytes.NewReader(exeBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Add("Idempotency-Key", "1") // lets Transport do retries with a POST
		if testParam != "" {
			req.Header.Add("X-Argument", testParam)
		}
		req.GetBody = func() (io.ReadCloser, error) { return ioutil.NopCloser(bytes.NewReader(exeBytes)), nil }
		res, err := sandboxBackendClient().Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected response from backend: %v", res.Status)
		}
		var execRes sandboxtypes.Response
		if err := json.NewDecoder(res.Body).Decode(&execRes); err != nil {
			log.Printf("JSON decode error from backend: %v", err)
			return nil, errors.New("error parsing JSON from backend")
		}
		if execRes.Error != "" {
			return &response{Errors: execRes.Error}, nil
		}
		exitCode = execRes.ExitCode
		rec.Stdout().Write(execRes.Stdout)
		rec.Stderr().Write(execRes.Stderr)
	} else {
		cmd := exec.CommandContext(runCtx, "sel_ldr_x86_64", "-l", "/dev/null", "-S", "-e", exe, testParam)
		cmd.Stdout = rec.Stdout()
		cmd.Stderr = rec.Stderr()
		if err := cmd.Run(); err != nil {
			if runCtx.Err() == context.DeadlineExceeded {
				// Send what was captured before the timeout.
				events, err := rec.Events()
				if err != nil {
					return nil, fmt.Errorf("error decoding events: %v", err)
				}
				return &response{Errors: "process took too long", Events: events}, nil
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				return nil, fmt.Errorf("error running sandbox: %v", err)
			}
			exitCode = exitErr.ExitCode()
		}
	}
	events, err := rec.Events()
	if err != nil {
		return nil, fmt.Errorf("error decoding events: %v", err)
	}
	var fails int
	if testParam != "" {
		// In case of testing the TestsFailed field contains how many tests have failed.
		for _, e := range events {
			fails += strings.Count(e.Message, failedTestPattern)
		}
	}
	var vetOut string
	if req.WithVet {
		// TODO: do this concurrently with the execution to reduce latency.
		vetOut, err = vetCheckInDir(tmpDir, goPath, useModules)
		if err != nil {
			return nil, fmt.Errorf("running vet: %v", err)
		}
	}
	return &response{
		Events:      events,
		Status:      exitCode,
		IsTest:      testParam != "",
		TestsFailed: fails,
		VetErrors:   vetOut,
		VetOK:       req.WithVet && vetOut == "",
	}, nil
}

// allowModuleDownloads reports whether the code snippet in src should be allowed
// to download modules.
func allowModuleDownloads(files *fileSet) bool {
	if files.Num() == 1 && bytes.Contains(files.Data(progName), []byte(`"code.google.com/p/go-tour/`)) {
		// This domain doesn't exist anymore but we want old snippets using
		// these packages to still run, so the Dockerfile adds these packages
		// at this name in $GOPATH. Any snippets using this old name wouldn't
		// have expected (or been able to use) third-party packages anyway,
		// so disabling modules and proxy fetches is acceptable.
		return false
	}
	v, _ := strconv.ParseBool(os.Getenv("ALLOW_PLAY_MODULE_DOWNLOADS"))
	return v
}

// playgroundGoproxy returns the GOPROXY environment config the playground should use.
// It is fetched from the environment variable PLAY_GOPROXY. A missing or empty
// value for PLAY_GOPROXY returns the default value of https://proxy.golang.org.
func playgroundGoproxy() string {
	proxypath := os.Getenv("PLAY_GOPROXY")
	if proxypath != "" {
		return proxypath
	}
	return "https://proxy.golang.org"
}

func (s *server) healthCheck() error {
	ctx := context.Background() // TODO: cap it to some reasonable timeout
	resp, err := compileAndRun(ctx, &request{Body: healthProg})
	if err != nil {
		return err
	}
	if resp.Errors != "" {
		return fmt.Errorf("compile error: %v", resp.Errors)
	}
	if len(resp.Events) != 1 || resp.Events[0].Message != "ok" {
		return fmt.Errorf("unexpected output: %v", resp.Events)
	}
	return nil
}

// sandboxBackendURL returns the URL of the sandbox backend that
// executes binaries. This backend is required for Go 1.14+ (where it
// executes using gvisor, since Native Client support is removed).
//
// This function either returns a non-empty string or it panics.
func sandboxBackendURL() string {
	if v := os.Getenv("SANDBOX_BACKEND_URL"); v != "" {
		return v
	}
	id, _ := metadata.ProjectID()
	switch id {
	case "golang-org":
		return "http://sandbox.play-sandbox-fwd.il4.us-central1.lb.golang-org.internal/run"
	}
	panic(fmt.Sprintf("no SANDBOX_BACKEND_URL environment and no default defined for project %q", id))
}

var sandboxBackendOnce struct {
	sync.Once
	c *http.Client
}

func sandboxBackendClient() *http.Client {
	sandboxBackendOnce.Do(initSandboxBackendClient)
	return sandboxBackendOnce.c
}

// initSandboxBackendClient runs from a sync.Once and initializes
// sandboxBackendOnce.c with the *http.Client we'll use to contact the
// sandbox execution backend.
func initSandboxBackendClient() {
	id, _ := metadata.ProjectID()
	switch id {
	case "golang-org":
		// For production, use a funky Transport dialer that
		// contacts backend directly, without going through an
		// internal load balancer, due to internal GCP
		// reasons, which we might resolve later. This might
		// be a temporary hack.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		rigd := gcpdial.NewRegionInstanceGroupDialer("golang-org", "us-central1", "play-sandbox-rigm")
		tr.DialContext = func(ctx context.Context, netw, addr string) (net.Conn, error) {
			if addr == "sandbox.play-sandbox-fwd.il4.us-central1.lb.golang-org.internal:80" {
				ip, err := rigd.PickIP(ctx)
				if err != nil {
					return nil, err
				}
				addr = net.JoinHostPort(ip, "80") // and fallthrough
			}
			var d net.Dialer
			return d.DialContext(ctx, netw, addr)
		}
		sandboxBackendOnce.c = &http.Client{Transport: tr}
	default:
		sandboxBackendOnce.c = http.DefaultClient
	}
}

const healthProg = `
package main

import "fmt"

func main() { fmt.Print("ok") }
`
