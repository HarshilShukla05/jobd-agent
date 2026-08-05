package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Screen is a simulated recruiter/AI-screener verdict on a resume. Modern
// pipelines rarely stop at keyword matching — an LLM (or a human skimming for
// ~10 seconds) makes the shortlist call. atsmatch covers the parser layer;
// this covers the judgment layer.
type Screen struct {
	Verdict       string   `json:"verdict"`         // advance | maybe | reject
	FitScore      int      `json:"fit_score"`       // 0-100
	TenSecondRead string   `json:"ten_second_read"` // what a skimming recruiter takes away
	Strengths     []string `json:"strengths"`
	Concerns      []string `json:"concerns"`
	MissingVsJD   []string `json:"missing_vs_jd"`
	Improvements  []string `json:"improvements"` // concrete, resume-only changes
}

const screenPrompt = `You are a technical recruiter screening resumes for the role below. Screen the way
real recruiters demonstrably do (eye-tracking research):

- You spend about 7 seconds on the first pass. Judge the TOP THIRD hardest: name, current
  title, current company, previous title/company, the dates of each (you check for steady
  progression), and education.
- Short bullets get read. Dense paragraphs get skipped — say so if you see them.
- You would search this candidate out of a database using terms from the JD; note any JD
  term you would search for and could not find.

You see hundreds of resumes, you are decisive, and you do not flatter. Judge ONLY what is
on this resume against this job description.

Return ONLY a JSON object:
{"verdict":"advance|maybe|reject","fit_score":0-100,"ten_second_read":string,
 "strengths":[string],"concerns":[string],"missing_vs_jd":[string],"improvements":[string]}

- verdict: would you pass this to the hiring manager?
- fit_score: 0-100 overall fit for THIS role.
- ten_second_read: what you'd take away skimming the top third only.
- concerns: what would make you hesitate, including seniority or stack gaps.
- missing_vs_jd: requirements you cannot verify from this resume.
- improvements: concrete changes to THIS resume's wording, ordering or emphasis.
  Do NOT suggest the candidate acquire new skills or invent experience.

JOB DESCRIPTION:
%s

RESUME:
%s`

// ScreenResume asks the model to act as the recruiter making the shortlist call.
func (g *Gemini) ScreenResume(ctx context.Context, jdText, resumeText string) (Screen, error) {
	if len(jdText) > 12000 {
		jdText = jdText[:12000]
	}
	if len(resumeText) > 12000 {
		resumeText = resumeText[:12000]
	}
	raw, err := g.generate(ctx, fmt.Sprintf(screenPrompt, jdText, resumeText))
	if err != nil {
		return Screen{}, err
	}
	var s Screen
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &s); err != nil {
		return Screen{}, fmt.Errorf("screen returned invalid JSON: %w", err)
	}
	return s, nil
}
