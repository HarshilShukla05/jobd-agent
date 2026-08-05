// Package atsmatch scores a rendered resume against a job description the way
// a keyword-matching ATS would: extract the terms the JD actually asks for,
// weight the ones stated as requirements, then check which of them survive in
// the resume's extracted text.
//
// Deterministic on purpose — no LLM. Higher coverage is always better: it is
// what makes the resume retrievable when a recruiter boolean-searches the ATS.
// The MISSING list is the actionable part — it names exactly which truthful
// bullets the selector should pull in to raise the score.
package atsmatch

import (
	"regexp"
	"sort"
	"strings"
)

// vocab maps a canonical skill term to the surface forms that mean the same
// thing. A resume containing any variant counts as covering the term.
var vocab = map[string][]string{
	"go":                  {"golang", "go"},
	"typescript":          {"typescript"},
	"javascript":          {"javascript"},
	"node.js":             {"node.js", "nodejs", "node js", "node"},
	"python":              {"python"},
	"java":                {"java"},
	"c++":                 {"c++"},
	"rust":                {"rust"},
	"ruby":                {"ruby"},
	"php":                 {"php"},
	"scala":               {"scala"},
	"kotlin":              {"kotlin"},
	"sql":                 {"sql"},
	"nosql":               {"nosql", "mongodb", "bigquery", "dynamodb", "cassandra"},
	"graphql":             {"graphql"},
	"rest":                {"rest", "restful"},
	"grpc":                {"grpc"},
	"websocket":           {"websocket"},
	"postgresql":          {"postgresql", "postgres"},
	"mysql":               {"mysql"},
	"mongodb":             {"mongodb", "mongo"},
	"redis":               {"redis"},
	"elasticsearch":       {"elasticsearch"},
	"kafka":               {"kafka"},
	"rabbitmq":            {"rabbitmq"},
	"pub/sub":             {"pub/sub", "pubsub", "pub sub"},
	"spanner":             {"spanner"},
	"bigquery":            {"bigquery"},
	"snowflake":           {"snowflake"},
	"dynamodb":            {"dynamodb"},
	"cassandra":           {"cassandra"},
	"aws":                 {"aws", "amazon web services"},
	"gcp":                 {"gcp", "google cloud"},
	"azure":               {"azure"},
	"kubernetes":          {"kubernetes", "k8s", "gke", "eks"},
	"docker":              {"docker", "containeri*"},
	"terraform":           {"terraform"},
	"jenkins":             {"jenkins"},
	"github actions":      {"github actions"},
	"api documentation":   {"swagger", "openapi", "api documentation"},
	"ci/cd":               {"ci/cd", "cicd", "continuous integration", "continuous delivery"},
	"devops":              {"devops"},
	"microservices":       {"microservice*"},
	"distributed systems": {"distributed system*"},
	"system design":       {"system design", "architect*"},
	"scalability":         {"scalab*", "scaling", "scale", "high-scale", "high scale"},
	"performance":         {"performance", "latency", "p99", "optimiz*"},
	"reliability":         {"reliab*", "fault-toleran*", "fault toleran*", "resilien*"},
	"security":            {"security", "secure", "authoriz*", "unauthoriz*", "authentic*", "oauth"},
	"testing":             {"test*", "jest"},
	"caching":             {"cach*"},
	"api design":          {"api", "apis"},
	"llm":                 {"llm", "large language model"},
	"rag":                 {"rag", "retrieval-augmented", "retrieval augmented"},
	"ai agents":           {"agentic", "ai agent", "intelligent agent"},
	"nlp":                 {"nlp", "natural language"},
	"embeddings":          {"embedding*"},
	"vector database":     {"vector database", "pinecone", "vector db"},
	"machine learning":    {"machine learning", "scikit*", "ml model"},
	"openai":              {"openai"},
	"react":               {"react"},
	"nextjs":              {"next.js", "nextjs"},
	"vue":                 {"vue"},
	"angular":             {"angular"},
	"html":                {"html"},
	"css":                 {"css"},
	"tailwind":            {"tailwind"},
	"webpack":             {"webpack"},
	"vite":                {"vite"},
	"express":             {"express"},
	"nestjs":              {"nestjs", "nest.js"},
	"fastify":             {"fastify"},
	"nx":                  {"nx monorepo", "nx"},
	"django":              {"django"},
	"flask":               {"flask"},
	"spring":              {"spring boot", "spring"},
	"temporal":            {"temporal"},
	"airflow":             {"airflow"},
	"spark":               {"spark"},
	"dataflow":            {"dataflow", "apache beam"},
	"etl":                 {"etl", "data pipeline*"},
	"webhooks":            {"webhook*"},
	"event-driven":        {"event-driven", "event driven"},
	"queues":              {"queue*"},
	"automation":          {"automat*"},
	"monitoring":          {"monitor*", "observab*"},
	"linux":               {"linux", "unix"},
	"git":                 {"git", "github", "gitlab"},
	"agile":               {"agile", "scrum"},
	"code review":         {"code review*", "peer review*"},
	"data structures":     {"data structure*", "algorithm*", "dsa"},
}

// requirement sections carry the terms a recruiter actually screens on
var reqSectionRe = regexp.MustCompile(`(?is)\b(requirements?|qualifications?|skills|must have|what you.?ll need|you have|looking for)\b(.*)`)

type Term struct {
	Name     string
	Weight   int // 2 if stated in a requirements section, else 1
	Covered  bool
	Evidence string // where it was found in the resume
}

type Report struct {
	Terms      []Term
	Score      float64 // weighted coverage, 0-100
	Covered    int
	Total      int
	Missing    []string
	MissingReq []string // missing AND stated as a requirement — the costly ones
}

func norm(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "’", "'")
	return regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
}

// matchCache compiles one regexp per surface form. A form ending in "*"
// matches as a prefix (optimiz* -> optimize, optimization); every other form
// must match as a whole word, so "scala" never matches inside "scalable".
var matchCache = map[string]*regexp.Regexp{}

func matcher(form string) *regexp.Regexp {
	if re, ok := matchCache[form]; ok {
		return re
	}
	prefix := strings.HasSuffix(form, "*")
	lit := regexp.QuoteMeta(strings.TrimSuffix(form, "*"))
	pat := `\b` + lit + `\b`
	if prefix {
		pat = `\b` + lit + `[a-z]*`
	}
	re := regexp.MustCompile(pat)
	matchCache[form] = re
	return re
}

func contains(hay, needle string) bool { return matcher(needle).MatchString(hay) }

// Score compares a JD against resume text (as extracted from the PDF, i.e.
// what an ATS actually sees).
func Score(jdText, resumeText string) Report {
	jd := norm(jdText)
	resume := norm(resumeText)

	reqBlock := jd
	if m := reqSectionRe.FindStringSubmatch(jd); m != nil {
		reqBlock = m[2]
	}

	var r Report
	for canonical, forms := range vocab {
		inJD := false
		for _, f := range forms {
			if contains(jd, f) {
				inJD = true
				break
			}
		}
		if !inJD {
			continue
		}
		t := Term{Name: canonical, Weight: 1}
		for _, f := range forms {
			if contains(reqBlock, f) {
				t.Weight = 2
				break
			}
		}
		for _, f := range forms {
			if contains(resume, f) {
				t.Covered = true
				t.Evidence = f
				break
			}
		}
		r.Terms = append(r.Terms, t)
	}

	var got, want int
	for _, t := range r.Terms {
		want += t.Weight
		if t.Covered {
			got += t.Weight
			r.Covered++
		} else {
			r.Missing = append(r.Missing, t.Name)
			if t.Weight == 2 {
				r.MissingReq = append(r.MissingReq, t.Name)
			}
		}
	}
	r.Total = len(r.Terms)
	if want > 0 {
		r.Score = float64(got) / float64(want) * 100
	}
	sort.Slice(r.Terms, func(i, j int) bool {
		if r.Terms[i].Covered != r.Terms[j].Covered {
			return !r.Terms[i].Covered // missing first — that's the actionable part
		}
		if r.Terms[i].Weight != r.Terms[j].Weight {
			return r.Terms[i].Weight > r.Terms[j].Weight
		}
		return r.Terms[i].Name < r.Terms[j].Name
	})
	sort.Strings(r.Missing)
	sort.Strings(r.MissingReq)
	return r
}

// Verdict turns the score into guidance for the apply stage.
func (r Report) Verdict() string {
	switch {
	case r.Score >= 85:
		return "STRONG — submit"
	case r.Score >= 70:
		return "GOOD — submit; add any missing term you can back truthfully"
	case r.Score >= 55:
		return "WEAK — revise the selection before submitting"
	default:
		return "POOR — likely a genuine stack mismatch, not a wording problem"
	}
}
