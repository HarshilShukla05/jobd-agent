// Package policy turns the facts the LLM gate extracted into a verdict. It
// lives in code, never in a prompt: the model only ever reports what the JD
// says, and this decides what that means for Harshil.
//
// Both entry points share it — the sweep's gate stage and the manual intake
// path — so a link he pastes is judged by exactly the same rules as a posting
// the daemon found on its own.
package policy

import (
	"fmt"
	"strings"

	"github.com/harshil/agent/internal/llm"
)

// YoE policy: Harshil has ~1 year. Roles at or below that are the preferred
// target; 1-2 years is a reasonable stretch worth applying to (the JD gate
// has already read the real requirement, and "2 years" postings routinely
// hire 1-year candidates). Above 2 years is a hard reject — v1 proved those
// get same-day rejections.
const (
	PreferredYoE = 1.0
	MaxYoE       = 2.0

	// SalaryFloorLPA — a stated band topping out below this is not worth the form.
	SalaryFloorLPA = 16.0
)

var profileStack = []string{"go", "golang", "node", "node.js", "javascript", "java", "c++",
	"grpc", "rest", "gRPC", "websocket", "redis", "postgres", "postgresql", "spanner", "sql", "postgresql",
	"gcp", "google cloud", "kubernetes", "docker", "pub/sub", "pubsub", "dataflow",
	"apache beam", "temporal", "microservices", "distributed systems"}

// Apply returns the status a job should land in, the human-readable reason,
// and its queue score.
func Apply(v llm.Verdict) (status, note string, score int) {
	if v.YoeMin != nil && *v.YoeMin > MaxYoE {
		return "rejected_hard", fmt.Sprintf("YoE gate: min %.1f > %.1f (%q)", *v.YoeMin, MaxYoE, v.YoeEvidence), 0
	}
	if v.LocationIndia != nil && !*v.LocationIndia {
		return "rejected_hard", "JD states non-India location", 0
	}
	if v.SalaryMaxLPA != nil && *v.SalaryMaxLPA < SalaryFloorLPA {
		return "rejected_hard", fmt.Sprintf("salary band max %.0f LPA < %.0f floor", *v.SalaryMaxLPA, SalaryFloorLPA), 0
	}
	for _, s := range v.Stack {
		for _, p := range profileStack {
			if strings.EqualFold(strings.TrimSpace(s), p) {
				score += 10
				break
			}
		}
	}
	if score > 50 {
		score = 50
	}
	switch {
	case v.YoeMin == nil:
		score += 5
		note = "no YoE stated — verify at apply time"
	case *v.YoeMin <= PreferredYoE: // squarely in range
		score += 20
		note = fmt.Sprintf("YoE ideal: %.1f (%q)", *v.YoeMin, v.YoeEvidence)
	default: // 1-2 years: a stretch, still worth applying to
		score += 10
		note = fmt.Sprintf("YoE stretch: %.1f vs his ~1yr (%q)", *v.YoeMin, v.YoeEvidence)
	}
	if v.LocationIndia != nil && *v.LocationIndia {
		score += 10
	}
	return "shortlisted", note, score
}
