package main

// Command epmon init generates a config file through the same schema the
// loader validates (spec §17.2): interactive prompt loop by default, flags
// with --non-interactive for scripts. Generation builds a YAML document,
// validates it through config.Parse (the loader path), and writes it
// atomically. Invalid documents are never written; existing files are
// never overwritten without --force; secrets are written as ${VAR}
// references, never literally.
//
//	Usage:
//	  epmon init [--output epmon.yaml] [--force]
//	  epmon init --non-interactive [--output epmon.yaml] [--force]
//	    [--server-listen :8080] [--db-dsn epmon.db] [--retention-days 90]
//	    [--default-interval 60s] [--default-timeout 10s] [--failure-threshold 1]
//	    --service id=url[;key=value...] ...

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"gopkg.in/yaml.v3"
)

// initService holds one service as collected; empty interval/timeout mean
// "inherit the probe defaults", empty name means "same as id".
type initService struct {
	id, name, url     string
	interval, timeout string
	expect, body      string
	headers           [][2]string
}

// initDoc is the collected answers before rendering.
type initDoc struct {
	output                  string
	listen, dsn             string
	retention, threshold    int
	defInterval, defTimeout string
	services                []initService
}

// serviceFlag accumulates repeatable --service values.
type serviceFlag []string

func (s *serviceFlag) String() string     { return strings.Join(*s, ", ") }
func (s *serviceFlag) Set(v string) error { *s = append(*s, v); return nil }

// initCommand runs epmon init. Exit codes: 0 wrote a valid file, 1 the
// document was invalid/refused/aborted, 64 usage.
func initCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	output := fs.String("output", "epmon.yaml", "destination file")
	force := fs.Bool("force", false, "overwrite an existing file")
	nonInteractive := fs.Bool("non-interactive", false, "drive from flags, no prompts")
	listen := fs.String("server-listen", ":8080", "server.listen")
	dsn := fs.String("db-dsn", "epmon.db", "sqlite database file")
	retention := fs.Int("retention-days", 90, "database.retention_days")
	defInterval := fs.String("default-interval", "60s", "probes.default_interval")
	defTimeout := fs.String("default-timeout", "10s", "probes.default_timeout")
	threshold := fs.Int("failure-threshold", 1, "probes.failure_threshold")
	var services serviceFlag
	fs.Var(&services, "service", "id=url[;key=value...]; repeatable (keys: name,interval,timeout,expect,body_contains)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "epmon: init takes no positional args\n")
		return exitUsage
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var doc *initDoc
	var err error
	if *nonInteractive {
		doc, err = initFromFlags(*output, *listen, *dsn, *retention, *defInterval, *defTimeout, *threshold, services)
	} else {
		// Interactive honors --output/--force only; content flags imply
		// the user meant --non-interactive.
		for name := range set {
			if name != "output" && name != "force" {
				fmt.Fprintf(stderr, "epmon: init flag --%s needs --non-interactive (or run without flags for prompts)\n", name)
				return exitUsage
			}
		}
		doc, err = initInteractive(stdin, stdout, *output)
	}
	if err != nil {
		fmt.Fprintf(stderr, "epmon: init: %v\n", err)
		return exitConfig
	}
	if err := writeInitDoc(doc, *force, stdout); err != nil {
		fmt.Fprintf(stderr, "epmon: init: %v\n", err)
		return exitConfig
	}
	return exitOK
}

// ---- non-interactive ----

// initFromFlags builds the document purely from flag values. Zero
// --service flags is fatal: a scripted run with no services is a
// scripting bug, and the loader rejects zero-service files anyway.
func initFromFlags(output, listen, dsn string, retention int, defInterval, defTimeout string, threshold int, services serviceFlag) (*initDoc, error) {
	if len(services) == 0 {
		return nil, fmt.Errorf("--non-interactive needs at least one --service (config files with zero services are invalid)")
	}
	doc := &initDoc{
		output: output, listen: listen, dsn: dsn,
		retention: retention, defInterval: defInterval,
		defTimeout: defTimeout, threshold: threshold,
	}
	for _, raw := range services {
		svc, err := parseServiceFlag(raw)
		if err != nil {
			return nil, err
		}
		doc.services = append(doc.services, svc)
	}
	if err := checkInitScalars(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// parseServiceFlag parses id=url[;key=value...]. Keys: name, interval,
// timeout, expect, body_contains. Headers are interactive-only.
func parseServiceFlag(raw string) (initService, error) {
	var svc initService
	head, rest, _ := strings.Cut(raw, ";")
	id, target, ok := strings.Cut(head, "=")
	svc.id = strings.TrimSpace(id)
	svc.url = strings.TrimSpace(target)
	if !ok || svc.id == "" || svc.url == "" {
		return svc, fmt.Errorf("--service %q must look like id=https://host/path[;key=value...]", raw)
	}
	for _, kv := range strings.Split(rest, ";") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return svc, fmt.Errorf("--service %q: %q needs key=value", raw, kv)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "name":
			svc.name = v
		case "interval":
			svc.interval = v
		case "timeout":
			svc.timeout = v
		case "expect":
			svc.expect = v
		case "body_contains":
			svc.body = v
		default:
			return svc, fmt.Errorf("--service %q: unknown key %q (want name|interval|timeout|expect|body_contains)", raw, k)
		}
	}
	return svc, nil
}

// checkInitScalars fails fast on malformed numbers/durations/URLs so a
// script gets a clear error; ranges remain the loader's job at the gate.
func checkInitScalars(doc *initDoc) error {
	if doc.retention < 1 {
		return fmt.Errorf("retention-days must be >= 1")
	}
	if doc.threshold < 1 {
		return fmt.Errorf("failure-threshold must be >= 1")
	}
	for _, d := range []string{doc.defInterval, doc.defTimeout} {
		if _, err := time.ParseDuration(d); err != nil {
			return fmt.Errorf("bad duration %q: %v", d, err)
		}
	}
	for i, s := range doc.services {
		if err := checkInitService(s); err != nil {
			return fmt.Errorf("service %d: %v", i, err)
		}
	}
	return nil
}

func checkInitService(s initService) error {
	if _, err := time.ParseDuration(orDefault(s.interval, "60s")); err != nil {
		return fmt.Errorf("bad interval %q: %v", s.interval, err)
	}
	if _, err := time.ParseDuration(orDefault(s.timeout, "10s")); err != nil {
		return fmt.Errorf("bad timeout %q: %v", s.timeout, err)
	}
	if _, err := parseExpect(orDefault(s.expect, "200")); err != nil {
		return fmt.Errorf("bad expect %q: %v", s.expect, err)
	}
	return checkURL(s.url)
}

func checkURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url must be absolute http(s), got %q", raw)
	}
	return nil
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ---- interactive ----

// prompter reads answer lines; EOF aborts the run with errAbort.
type prompter struct {
	sc  *bufio.Scanner
	out io.Writer
}

var errAbort = fmt.Errorf("aborted (no file written)")

func (p *prompter) ask(prompt, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", prompt)
	}
	if !p.sc.Scan() {
		if err := p.sc.Err(); err != nil {
			return "", err
		}
		return "", errAbort
	}
	ans := strings.TrimSpace(p.sc.Text())
	if ans == "" {
		return def, nil
	}
	return ans, nil
}

func (p *prompter) askNonEmpty(prompt string) (string, error) {
	for {
		ans, err := p.ask(prompt, "")
		if err != nil {
			return "", err
		}
		if ans != "" {
			return ans, nil
		}
	}
}

// initInteractive walks the prompts. A first empty service id re-prompts
// (zero-service files are invalid); EOF anywhere aborts with no output.
func initInteractive(stdin io.Reader, stdout io.Writer, output string) (*initDoc, error) {
	p := &prompter{sc: bufio.NewScanner(stdin), out: stdout}
	p.sc.Buffer(make([]byte, 64*1024), 1024*1024)
	fmt.Fprintln(stdout, "epmon init — answer prompts, empty takes [default]. Ctrl-C aborts.")

	doc := &initDoc{output: output}
	var err error
	if doc.listen, err = p.ask("Listen address", ":8080"); err != nil {
		return nil, err
	}
	if doc.dsn, err = p.ask("SQLite database file", "epmon.db"); err != nil {
		return nil, err
	}
	retention, err := askInt(p, "Retention days", 90, 1, 0)
	if err != nil {
		return nil, err
	}
	doc.retention = retention
	if doc.defInterval, err = askDuration(p, "Default probe interval", "60s"); err != nil {
		return nil, err
	}
	if doc.defTimeout, err = askDuration(p, "Default probe timeout", "10s"); err != nil {
		return nil, err
	}
	threshold, err := askInt(p, "Failure threshold", 1, 1, 0)
	if err != nil {
		return nil, err
	}
	doc.threshold = threshold

	for {
		id, err := p.ask("Service id (empty when done)", "")
		if err != nil {
			return nil, err
		}
		if id == "" {
			if len(doc.services) == 0 {
				fmt.Fprintln(stdout, "At least one service is required — a config with zero services is invalid.")
				continue
			}
			break
		}
		svc, err := askService(p, id)
		if err != nil {
			return nil, err
		}
		doc.services = append(doc.services, svc)
	}
	return doc, nil
}

func askInt(p *prompter, prompt string, def, min, max int) (int, error) {
	for {
		ans, err := p.ask(prompt, strconv.Itoa(def))
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(ans)
		if err != nil || n < min || (max > 0 && n > max) {
			if max > 0 {
				fmt.Fprintf(p.out, "Enter a number %d..%d.\n", min, max)
			} else {
				fmt.Fprintf(p.out, "Enter a number >= %d.\n", min)
			}
			continue
		}
		return n, nil
	}
}

func askDuration(p *prompter, prompt, def string) (string, error) {
	for {
		ans, err := p.ask(prompt, def)
		if err != nil {
			return "", err
		}
		if _, err := time.ParseDuration(ans); err != nil {
			fmt.Fprintf(p.out, "Bad duration %q (try 30s, 5m).\n", ans)
			continue
		}
		return ans, nil
	}
}

// askService collects one service; id was already given. URL and expect
// re-prompt on errors; ranges are enforced by the validate gate.
func askService(p *prompter, id string) (initService, error) {
	svc := initService{id: id}
	var err error
	if svc.name, err = p.ask("  Name", id); err != nil {
		return svc, err
	}
	for {
		if svc.url, err = p.askNonEmpty("  URL"); err != nil {
			return svc, err
		}
		if err := checkURL(svc.url); err != nil {
			fmt.Fprintf(p.out, "  %v.\n", err)
			continue
		}
		break
	}
	if svc.interval, err = askOptionalDuration(p, "  Interval (empty = default)"); err != nil {
		return svc, err
	}
	if svc.timeout, err = askOptionalDuration(p, "  Timeout (empty = default)"); err != nil {
		return svc, err
	}
	for {
		ans, err := p.ask("  Expected status (200, 2xx, 200,301)", "200")
		if err != nil {
			return svc, err
		}
		if _, err := parseExpect(ans); err != nil {
			fmt.Fprintf(p.out, "  %v.\n", err)
			continue
		}
		svc.expect = ans
		break
	}
	if svc.body, err = p.ask("  Body must contain (empty = skip)", ""); err != nil {
		return svc, err
	}
	for {
		h, err := p.ask(`  Header "Name: value" ($NAME keeps a secret out of the file, empty when done)`, "")
		if err != nil {
			return svc, err
		}
		if h == "" {
			break
		}
		name, value, ok := strings.Cut(h, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			fmt.Fprintln(p.out, `  Give "Name: value" or leave empty.`)
			continue
		}
		svc.headers = append(svc.headers, [2]string{name, secretRef(value)})
	}
	return svc, nil
}

// askOptionalDuration is askDuration with blank allowed (inherit).
func askOptionalDuration(p *prompter, prompt string) (string, error) {
	for {
		fmt.Fprintf(p.out, "%s: ", prompt)
		if !p.sc.Scan() {
			if err := p.sc.Err(); err != nil {
				return "", err
			}
			return "", errAbort
		}
		ans := strings.TrimSpace(p.sc.Text())
		if ans == "" {
			return "", nil
		}
		if _, err := time.ParseDuration(ans); err != nil {
			fmt.Fprintf(p.out, "Bad duration %q (try 30s, 5m).\n", ans)
			continue
		}
		return ans, nil
	}
}

// secretRef rewrites bare $NAME references to ${NAME} anywhere in the
// value — the substitution grammar (§4.2) only expands braced forms, so
// `Bearer $TOKEN` would otherwise reach the runtime literally. $$
// (literal $) and ${...} forms pass through untouched.
var bareRefRe = regexp.MustCompile(`\$(\$|[A-Za-z_][A-Za-z0-9_]*)`)

func secretRef(v string) string {
	return bareRefRe.ReplaceAllStringFunc(v, func(m string) string {
		if m == "$$" {
			return m
		}
		return "${" + m[1:] + "}"
	})
}

// parseExpect validates expect_status input through the loader's own
// §4.3.1 parser and returns the YAML node to emit: a bare scalar for a
// single int/class, a flow sequence otherwise.
func parseExpect(input string) (*yaml.Node, error) {
	toks := strings.Fields(strings.ReplaceAll(input, ",", " "))
	if len(toks) == 0 {
		return nil, fmt.Errorf("expect_status needs at least one status (200, 2xx, 200,301)")
	}
	items := make([]*yaml.Node, 0, len(toks))
	for _, t := range toks {
		if n, err := strconv.Atoi(t); err == nil {
			items = append(items, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)})
		} else {
			items = append(items, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: t})
		}
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items}
	var spec config.StatusSpec
	if err := spec.UnmarshalYAML(seq); err != nil {
		return nil, err
	}
	if len(items) == 1 {
		return items[0], nil
	}
	seq.Style = yaml.FlowStyle
	return seq, nil
}

// ---- render + gate + write ----

// renderInitDoc builds the YAML document: only keys init covers, with
// section comments. Everything else stays loader-defaulted.
func renderInitDoc(doc *initDoc) *yaml.Node {
	str := func(v string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	}
	num := func(n int) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)}
	}
	pair := func(k string, v *yaml.Node) []*yaml.Node {
		return []*yaml.Node{str(k), v}
	}
	mapping := func(pairs ...[]*yaml.Node) *yaml.Node {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, p := range pairs {
			m.Content = append(m.Content, p...)
		}
		return m
	}

	root := mapping()
	content := &root.Content

	server := mapping(
		pair("listen", str(doc.listen)),
	)
	server.HeadComment = "epmon API listen address."
	db := mapping(
		pair("driver", str("sqlite")),
		pair("dsn", str(doc.dsn)),
		pair("retention_days", num(doc.retention)),
	)
	db.HeadComment = "Storage: sqlite file plus history retention."
	probes := mapping(
		pair("default_interval", str(doc.defInterval)),
		pair("default_timeout", str(doc.defTimeout)),
		pair("failure_threshold", num(doc.threshold)),
	)
	probes.HeadComment = "Fleet probe defaults; services inherit unless overridden."

	svcs := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, s := range doc.services {
		expect, err := parseExpect(orDefault(s.expect, "200"))
		if err != nil {
			expect = str("200") // validated earlier; cannot happen
		}
		fields := []*yaml.Node{str("id"), str(s.id)}
		if s.name != "" && s.name != s.id {
			fields = append(fields, str("name"), str(s.name))
		}
		fields = append(fields, str("url"), str(s.url))
		if strings.TrimSpace(s.interval) != "" {
			fields = append(fields, str("interval"), str(s.interval))
		}
		if strings.TrimSpace(s.timeout) != "" {
			fields = append(fields, str("timeout"), str(s.timeout))
		}
		fields = append(fields, str("expect_status"), expect)
		if s.body != "" {
			fields = append(fields, str("body_contains"), str(s.body))
		}
		if len(s.headers) > 0 {
			hm := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for _, h := range s.headers {
				hm.Content = append(hm.Content, str(h[0]), str(h[1]))
			}
			fields = append(fields, str("headers"), hm)
		}
		svc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: fields}
		svcs.Content = append(svcs.Content, svc)
	}

	for _, kv := range [][]*yaml.Node{
		pair("server", server),
		pair("database", db),
		pair("probes", probes),
		{str("services"), svcs},
	} {
		*content = append(*content, kv...)
	}
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
}

// validateInitDoc marshals the rendered document and runs it through
// config.Parse — the exact loader path `run` uses. ${VAR} references the
// operator typed are stubbed during the check (and only the check) so
// substitution doesn't fail on values that will exist at runtime.
func validateInitDoc(doc *initDoc, node *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	restore := stubMissingEnv(stubEnvNames(buf.Bytes()))
	defer restore()
	if _, err := config.Parse(doc.output, buf.Bytes()); err != nil {
		return err
	}
	return nil
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

func stubEnvNames(raw []byte) []string {
	var missing []string
	for _, m := range envRefRe.FindAllSubmatch(raw, -1) {
		name := string(m[1])
		if _, ok := os.LookupEnv(name); !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

func stubMissingEnv(names []string) func() {
	prev := make(map[string]*string, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			p := v
			prev[n] = &p
		} else {
			prev[n] = nil
		}
		_ = os.Setenv(n, "epmon-init-placeholder")
	}
	return func() {
		for n, p := range prev {
			if p == nil {
				_ = os.Unsetenv(n)
			} else {
				_ = os.Setenv(n, *p)
			}
		}
	}
}

// writeInitDoc validates the rendered document, then writes it atomically
// (temp + rename in the destination directory). Existing files need
// --force; nothing is written on any failure.
func writeInitDoc(doc *initDoc, force bool, stdout io.Writer) error {
	if _, err := os.Stat(doc.output); err == nil && !force {
		return fmt.Errorf("%s exists (use --force to overwrite)", doc.output)
	}
	node := renderInitDoc(doc)
	if err := validateInitDoc(doc, node); err != nil {
		return fmt.Errorf("generated config is invalid: %v", err)
	}
	var buf bytes.Buffer
	buf.WriteString("# Generated by `epmon init` — edit freely, then validate:\n")
	buf.WriteString("#   epmon validate --config " + doc.output + "\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	dir := filepath.Dir(doc.output)
	tmp, err := os.CreateTemp(dir, ".epmon-init-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, doc.output); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (%d service(s)); verify with: epmon validate --config %s\n",
		doc.output, len(doc.services), doc.output)
	return nil
}
