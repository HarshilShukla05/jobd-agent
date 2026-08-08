// resumegen renders a tailored, ATS-safe resume PDF from the master experience
// bank plus a per-job selection file. The LLM only ever produces the selection
// (bullet IDs, ordering, approved aliases); all prose comes from the bank.
//
// Pipeline: validate selection against bank -> fill template.tex -> compile
// with tectonic -> assert exactly 1 page and every bullet survives text
// extraction (the ATS-parse guarantee). Any failure is a hard error so the
// caller can fall back to the baseline resume.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"context"

	"github.com/harshil/agent/internal/atsmatch"
	"github.com/harshil/agent/internal/llm"
	"gopkg.in/yaml.v3"
)

type Bank struct {
	Identity struct {
		Name     string `yaml:"name"`
		Phone    string `yaml:"phone"`
		Email    string `yaml:"email"`
		Linkedin string `yaml:"linkedin"`
		Github   string `yaml:"github"`
	} `yaml:"identity"`
	Summaries []struct {
		ID   string   `yaml:"id"`
		Text string   `yaml:"text"`
		Tags []string `yaml:"tags"`
	} `yaml:"summaries"`
	Education []struct {
		School   string `yaml:"school"`
		Location string `yaml:"location"`
		Degree   string `yaml:"degree"`
		Dates    string `yaml:"dates"`
	} `yaml:"education"`
	Experience   []Role              `yaml:"experience"`
	Projects     []Role              `yaml:"projects"`
	Skills       map[string][]string `yaml:"skills"`
	Achievements []struct {
		ID   string `yaml:"id"`
		Text string `yaml:"text"`
	} `yaml:"achievements"`
	Aliases     map[string][]string `yaml:"aliases"`
	Constraints struct {
		OnePage        bool `yaml:"one_page"`
		MaxBulletChars int  `yaml:"max_bullet_chars"`
	} `yaml:"constraints"`
}

type Role struct {
	ID         string   `yaml:"id"`
	Company    string   `yaml:"company"`
	Name       string   `yaml:"name"` // projects
	Location   string   `yaml:"location"`
	Title      string   `yaml:"title"`
	Dates      string   `yaml:"dates"`
	TechPool   []string `yaml:"tech_pool"`
	TechLine   string   `yaml:"tech_line"` // projects: fixed line
	RecruiterSafe *bool  `yaml:"recruiter_safe"` // nil = safe; false = never send out
	MinBullets int      `yaml:"min_bullets"`
	MaxBullets int      `yaml:"max_bullets"`
	Bullets    []Bullet `yaml:"bullets"`
}

type Bullet struct {
	ID        string   `yaml:"id"`
	Text      string   `yaml:"text"`
	Tags      []string `yaml:"tags"`
	Conflicts []string `yaml:"conflicts"` // same underlying work — never co-select
}

// Selection is what the LLM produces per job. Everything referenced here must
// exist in the bank; resumegen rejects anything it cannot resolve.
type Selection struct {
	JobID           string              `json:"job_id"`
	Bullets         map[string][]string `json:"bullets"`          // role id -> ordered bullet ids
	TechLines       map[string][]string `json:"tech_lines"`       // role id -> ordered subset of tech_pool
	Skills          map[string][]string `json:"skills"`           // category -> ordered subset of pool
	Aliases         []AliasSub          `json:"aliases"`          // approved term mirrors
	IncludeProjects []string            `json:"include_projects"` // ordered project ids
	Summary         string              `json:"summary"`          // id of a bank summary
}

type AliasSub struct {
	BulletID string `json:"bullet_id"`
	From     string `json:"from"`
	To       string `json:"to"`
}

type renderRole struct {
	Company, Name, Location, Title, TechLine, Dates string
	Bullets                                         []string
}

type renderData struct {
	Summary      string
	Identity     any
	Education    any
	Experience   []renderRole
	Projects     []renderRole
	Skills       struct{ Languages, CloudInfra, DataStorage, Systems string }
	Achievements []string
}

var latexEscaper = strings.NewReplacer(
	`\`, `\textbackslash{}`, `&`, `\&`, `%`, `\%`, `$`, `\$`, `#`, `\#`,
	`_`, `\_`, `{`, `\{`, `}`, `\}`, `~`, `\textasciitilde{}`, `^`, `\textasciicircum{}`,
	// LaTeX turns a plain ' into a curly U+2019 in the text layer, so "user's"
	// extracts as "user’s" and a literal search for the ASCII spelling misses
	// it. \textquotesingle keeps U+0027, which is what an ATS actually greps for.
	`'`, `\textquotesingle{}`,
)

func esc(s string) string { return latexEscaper.Replace(s) }

// Words too ordinary for a repeat to mean anything. Anything not listed here and
// at least 5 letters long is "distinctive" enough that saying it twice in one
// breath is waste rather than grammar.
var commonWords = map[string]bool{
	"across": true, "after": true, "against": true, "another": true, "before": true,
	"below": true, "between": true, "every": true, "from": true, "into": true,
	"other": true, "over": true, "their": true, "them": true, "then": true,
	"there": true, "these": true, "they": true, "this": true, "those": true,
	"through": true, "under": true, "until": true, "when": true, "where": true,
	"which": true, "while": true, "with": true, "without": true,
}

var wordRe = regexp.MustCompile(`[A-Za-z][A-Za-z.+/-]*`)

// significantWords returns the lowercased distinctive words in a line.
func significantWords(s string) []string {
	var out []string
	for _, w := range wordRe.FindAllString(s, -1) {
		w = strings.ToLower(strings.Trim(w, ".-/"))
		if len(w) >= 5 && !commonWords[w] {
			out = append(out, w)
		}
	}
	return out
}

// lintRepeats warns when a distinctive word is used twice inside one line, or in
// two lines that sit next to each other on the page.
//
// Harshil's rule, 2026-08-07: "Backend engineer ... on the backend of a chat app"
// spent two of the summary's ~35 words saying one thing. This is deliberately
// scoped to ONE line and its immediate neighbour — a keyword recurring across the
// whole page ("payments" in five bullets) is correct and helps the ATS.
//
// A warning, never a hard failure: a genuine repeat is occasionally the clearest
// phrasing, and that call is Harshil's. But it is printed, because the same rule
// written only in prose was already broken once — two pricing bullets shipped
// together both hanging on the word "six".
func lintRepeats(lines []string) {
	var warned []string
	for i, line := range lines {
		seen := map[string]bool{}
		for _, w := range significantWords(line) {
			if seen[w] {
				warned = append(warned, fmt.Sprintf(
					"  %q used twice in one line: %s", w, line))
				break
			}
			seen[w] = true
		}
		if i == 0 {
			continue
		}
		prev := map[string]bool{}
		for _, w := range significantWords(lines[i-1]) {
			prev[w] = true
		}
		for _, w := range significantWords(line) {
			if prev[w] {
				warned = append(warned, fmt.Sprintf(
					"  %q repeats from the line above:\n    %s\n    %s", w, lines[i-1], line))
				break
			}
		}
	}
	if len(warned) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\nrepeated wording (%d) — each repeat is a slot that "+
		"could carry a new fact:\n", len(warned))
	for _, w := range warned {
		fmt.Fprintln(os.Stderr, w)
	}
	fmt.Fprintln(os.Stderr)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "resumegen: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	bankPath := flag.String("bank", "bank/bank.yaml", "path to experience bank")
	selPath := flag.String("selection", "", "path to selection JSON (required)")
	tmplPath := flag.String("template", "resume/template.tex", "path to LaTeX template")
	outDir := flag.String("out", "out", "output directory")
	name := flag.String("name", "Harshil_Shukla_Resume", "output file base name")
	jdPath := flag.String("jd", "", "optional: path to the job description text; prints a screening report")
	screen := flag.Bool("screen", false, "with -jd: also run an AI recruiter screen (uses Gemini)")
	flag.Parse()
	if *selPath == "" {
		fail("-selection is required")
	}

	bank := mustBank(*bankPath)
	sel := mustSelection(*selPath)

	roleIndex := map[string]*Role{}
	for i := range bank.Experience {
		roleIndex[bank.Experience[i].ID] = &bank.Experience[i]
	}
	projIndex := map[string]*Role{}
	for i := range bank.Projects {
		projIndex[bank.Projects[i].ID] = &bank.Projects[i]
	}

	// --- validate + resolve ---
	var data renderData
	var plainBullets []string // pre-escape text for the ATS round-trip check
	var prose []string        // summary + bullet text only, in render order, for the repeat lint
	data.Identity = bank.Identity
	data.Education = bank.Education

	// the model picks a summary by id; it never supplies prose
	if sel.Summary != "" {
		var found bool
		for _, sm := range bank.Summaries {
			if sm.ID == sel.Summary {
				plainBullets = append(plainBullets, sm.Text)
				prose = append(prose, sm.Text)
				data.Summary = esc(sm.Text)
				found = true
				break
			}
		}
		if !found {
			fail("unknown summary id %q", sel.Summary)
		}
	}

	globalSel := map[string]bool{} // conflict pairs can span roles
	for _, ids := range sel.Bullets {
		for _, id := range ids {
			globalSel[id] = true
		}
	}

	resolveRole := func(r *Role, isProject bool) renderRole {
		ids := sel.Bullets[r.ID]
		if len(ids) < r.MinBullets || len(ids) > r.MaxBullets {
			fail("role %q: %d bullets selected, budget is %d-%d", r.ID, len(ids), r.MinBullets, r.MaxBullets)
		}
		byID := map[string]Bullet{}
		for _, b := range r.Bullets {
			byID[b.ID] = b
		}
		seen := map[string]bool{}
		var texts []string
		for _, id := range ids {
			b, ok := byID[id]
			if !ok {
				fail("role %q: unknown bullet id %q", r.ID, id)
			}
			if seen[id] {
				fail("role %q: duplicate bullet id %q", r.ID, id)
			}
			seen[id] = true
			for _, c := range b.Conflicts {
				if globalSel[c] {
					fail("bullets %q and %q describe the same work — select only one", id, c)
				}
			}
			text := applyAliases(b.Text, id, sel.Aliases, bank.Aliases)
			plainBullets = append(plainBullets, text)
			prose = append(prose, text)
			texts = append(texts, esc(text))
		}
		techLine := r.TechLine
		if !isProject {
			techLine = resolveTechLine(r, sel.TechLines[r.ID])
		}
		// company, title and tech line are as ATS-critical as the bullets —
		// a kern split in any of them is invisible to a keyword search
		plainBullets = append(plainBullets, r.Company, r.Title, techLine)
		if isProject {
			plainBullets = append(plainBullets, r.Name)
		}
		return renderRole{
			Company: esc(r.Company), Name: esc(r.Name), Location: esc(r.Location),
			Title: esc(r.Title), TechLine: esc(techLine), Dates: esc(r.Dates), Bullets: texts,
		}
	}

	for i := range bank.Experience {
		data.Experience = append(data.Experience, resolveRole(&bank.Experience[i], false))
	}
	projects := sel.IncludeProjects
	if len(projects) == 0 {
		fail("include_projects is empty — at least one project required")
	}
	for _, pid := range projects {
		p, ok := projIndex[pid]
		if !ok {
			fail("unknown project id %q", pid)
		}
		if p.RecruiterSafe != nil && !*p.RecruiterSafe {
			fail("project %q is marked recruiter_safe: false — it must never appear "+
				"on a resume sent to a company", pid)
		}
		data.Projects = append(data.Projects, resolveRole(p, true))
	}

	data.Skills.Languages = resolveSkills(bank, sel, "languages")
	data.Skills.CloudInfra = resolveSkills(bank, sel, "cloud_infra")
	data.Skills.DataStorage = resolveSkills(bank, sel, "data_storage")
	data.Skills.Systems = resolveSkills(bank, sel, "systems")
	// every individual skill token must survive extraction — this is where the
	// "AWS" -> "A WS" kern split was found
	for _, cat := range []string{"languages", "cloud_infra", "data_storage", "systems"} {
		plainBullets = append(plainBullets, sel.Skills[cat]...)
	}

	// contact details and education: a broken email or phone costs a callback
	plainBullets = append(plainBullets,
		bank.Identity.Name, bank.Identity.Email, bank.Identity.Phone,
		bank.Identity.Linkedin, bank.Identity.Github)
	for _, e := range bank.Education {
		plainBullets = append(plainBullets, e.School, e.Degree)
	}

	for _, a := range bank.Achievements {
		plainBullets = append(plainBullets, a.Text)
		data.Achievements = append(data.Achievements, esc(a.Text))
	}

	// --- render ---
	tmpl, err := template.New(filepath.Base(*tmplPath)).Delims("<<", ">>").ParseFiles(*tmplPath)
	if err != nil {
		fail("template parse: %v", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fail("mkdir: %v", err)
	}
	texPath := filepath.Join(*outDir, *name+".tex")
	f, err := os.Create(texPath)
	if err != nil {
		fail("create tex: %v", err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		fail("template execute: %v", err)
	}
	f.Close()

	// --- compile ---
	tectonic := findBin("tectonic", "tools/tectonic")
	cmd := exec.Command(tectonic, "--outdir", *outDir, texPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		fail("tectonic failed: %v\n%s", err, out)
	}
	pdfPath := filepath.Join(*outDir, *name+".pdf")

	// --- verify (1 page + ATS text round-trip) ---
	// Strict check on every capitalised/technical TOKEN inside the strings, not
	// just whole strings without spaces. The bug that motivated this lived at
	// "AWS" inside "AWS fundamentals (EC2, S3, IAM)": kerning split it to "A WS"
	// in the text layer, and the whitespace-stripped compare could not see it.
	var exact []string
	seenTok := map[string]bool{}
	for _, s := range plainBullets {
		for _, w := range strings.Fields(s) {
			w = strings.Trim(w, "(),.;:\u2019'\"/")
			if len([]rune(w)) < 2 || seenTok[w] {
				continue
			}
			// tokens an ATS would keyword-search: any internal capital or digit
			if strings.ToLower(w) == w {
				continue
			}
			seenTok[w] = true
			exact = append(exact, w)
		}
	}
	expected := map[string]any{
		"max_pages":          1,
		"must_contain":       plainBullets,
		"must_contain_exact": exact,
	}
	expPath := filepath.Join(*outDir, *name+".expected.json")
	eb, _ := json.Marshal(expected)
	if err := os.WriteFile(expPath, eb, 0o644); err != nil {
		fail("write expected: %v", err)
	}
	vcmd := exec.Command("python3", "scripts/verify_pdf.py", pdfPath, expPath)
	if out, err := vcmd.CombinedOutput(); err != nil {
		fail("ATS verification FAILED:\n%s", out)
	}

	fmt.Printf("OK %s (1 page, %d bullets verified in extracted text)\n", pdfPath, len(plainBullets))

	if *jdPath != "" {
		reportMatch(*jdPath, pdfPath, *screen, bank)
	}
}

func mustBank(path string) *Bank {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("read bank: %v", err)
	}
	var bank Bank
	if err := yaml.Unmarshal(b, &bank); err != nil {
		fail("parse bank: %v", err)
	}
	return &bank
}

func mustSelection(path string) *Selection {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("read selection: %v", err)
	}
	var s Selection
	if err := json.Unmarshal(b, &s); err != nil {
		fail("parse selection: %v", err)
	}
	return &s
}

// applyAliases swaps approved terms only. A requested substitution must exist
// in the bank's alias map (in either direction) and the source term must
// actually appear in the bullet.
func applyAliases(text, bulletID string, subs []AliasSub, approved map[string][]string) string {
	for _, s := range subs {
		if s.BulletID != bulletID {
			continue
		}
		if !aliasApproved(s.From, s.To, approved) {
			fail("bullet %q: alias %q -> %q is not in the approved alias map", bulletID, s.From, s.To)
		}
		if !strings.Contains(text, s.From) {
			fail("bullet %q: alias source %q not present in bullet text", bulletID, s.From)
		}
		text = strings.Replace(text, s.From, s.To, 1)
	}
	return text
}

func aliasApproved(from, to string, approved map[string][]string) bool {
	for _, t := range approved[from] {
		if t == to {
			return true
		}
	}
	for _, f := range approved[to] { // reverse direction
		if f == from {
			return true
		}
	}
	return false
}

func resolveTechLine(r *Role, items []string) string {
	if len(items) == 0 {
		fail("role %q: tech_lines entry missing", r.ID)
	}
	pool := map[string]bool{}
	for _, t := range r.TechPool {
		pool[t] = true
	}
	for _, it := range items {
		if !pool[it] {
			fail("role %q: tech line item %q not in tech_pool", r.ID, it)
		}
	}
	return strings.Join(items, ", ")
}

func resolveSkills(bank *Bank, sel *Selection, category string) string {
	items := sel.Skills[category]
	if len(items) == 0 {
		fail("skills category %q: empty selection", category)
	}
	pool := map[string]bool{}
	for _, s := range bank.Skills[category] {
		pool[s] = true
	}
	var out []string
	for _, it := range items {
		if !pool[it] {
			fail("skills category %q: item %q not in pool", category, it)
		}
		out = append(out, esc(it))
	}
	return strings.Join(out, ", ")
}

func findBin(name, fallback string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if abs, err := filepath.Abs(fallback); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	fail("%s not found on PATH or at %s", name, fallback)
	return ""
}

// reportMatch scores the rendered PDF against the JD exactly as an ATS would:
// against the text extracted from the PDF, not the LaTeX source.
func reportMatch(jdPath, pdfPath string, screen bool, bank *Bank) {
	jd, err := os.ReadFile(jdPath)
	if err != nil {
		fail("read jd: %v", err)
	}
	out, err := exec.Command("python3", "scripts/pdf_text.py", pdfPath).Output()
	if err != nil {
		fail("extract pdf text: %v", err)
	}
	r := atsmatch.Score(string(jd), string(out))
	fmt.Printf("\nATS MATCH: %.0f%%  (%d/%d JD terms)  %s\n",
		r.Score, r.Covered, r.Total, r.Verdict())
	if len(r.MissingReq) > 0 {
		fmt.Printf("  missing REQUIRED: %s\n", strings.Join(r.MissingReq, ", "))
	}
	var missOpt []string
	for _, m := range r.Missing {
		req := false
		for _, mr := range r.MissingReq {
			if mr == m {
				req = true
			}
		}
		if !req {
			missOpt = append(missOpt, m)
		}
	}
	if len(missOpt) > 0 {
		fmt.Printf("  missing (nice-to-have): %s\n", strings.Join(missOpt, ", "))
	}
	fmt.Println("  -> raise this as high as the truth allows: recruiters boolean-search the")
	fmt.Println("     ATS, so every covered term is another query you surface in. Pull in")
	fmt.Println("     bank bullets that carry the missing terms; never invent one.")

	// Layer 2: the 7.4-second human skim — the step that actually shortlists.
	var title, company string
	if len(bank.Experience) > 0 {
		title, company = bank.Experience[0].Title, bank.Experience[0].Company
	}
	sk := atsmatch.Skim(string(out), bank.Identity.Name, title, company)
	fmt.Printf("\n7-SECOND SKIM: %d/%d checks passed\n", sk.Passed, sk.Total)
	for _, c := range sk.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Printf("  %s %-32s %s\n", mark, c.Name, c.Detail)
	}

	if !screen {
		return
	}
	// Layer 3: the AI screen (~82% of firms now use one).
	ctx := context.Background()
	gem, err := llm.NewGeminiFromEnv(ctx)
	if err != nil {
		fmt.Printf("\nAI recruiter screen skipped: %v\n", err)
		return
	}
	sc, err := gem.ScreenResume(ctx, string(jd), string(out))
	if err != nil {
		fmt.Printf("\nAI recruiter screen failed: %v\n", err)
		return
	}
	fmt.Printf("\nAI RECRUITER SCREEN: %s (fit %d/100)\n", strings.ToUpper(sc.Verdict), sc.FitScore)
	fmt.Printf("  10-second read: %s\n", sc.TenSecondRead)
	for _, x := range sc.Strengths {
		fmt.Printf("  +    %s\n", x)
	}
	for _, x := range sc.Concerns {
		fmt.Printf("  -    %s\n", x)
	}
	for _, x := range sc.MissingVsJD {
		fmt.Printf("  ?    missing: %s\n", x)
	}
	for _, x := range sc.Improvements {
		fmt.Printf("  fix> %s\n", x)
	}
}
