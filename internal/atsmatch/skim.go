package atsmatch

import (
	"fmt"
	"regexp"
	"strings"
)

// The real screening funnel, per sourced research (see RESEARCH.md):
//
//  1. KEYWORD COVERAGE — recruiters query the ATS by skill and title, so a JD
//     term the resume lacks makes it unretrievable. Score() measures this, and
//     it should be pushed as high as the truth allows on every application.
//  2. KNOCKOUT QUESTIONS — required application-form answers (YoE, work
//     authorization, relocation, salary) can reject without human review, so
//     they carry independent risk and must never be guessed.
//  3. AI SCREEN — ~82% of firms now use AI to sift resumes; judges contextual fit.
//  4. THE 7.4-SECOND HUMAN SKIM — the actual shortlist decision. Eye-tracking
//     shows recruiters fixate on six things: name, current title, current
//     company, previous title/company, dates of each, and education. Short
//     bullets get read; dense paragraphs are skipped.
//
// Skim scores layer 4, which is the one most "ATS optimizers" ignore entirely.

type SkimReport struct {
	Checks []SkimCheck
	Passed int
	Total  int
}

type SkimCheck struct {
	Name   string
	OK     bool
	Detail string
}

var (
	dateRe    = regexp.MustCompile(`(?i)(19|20)\d{2}|jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec|present`)
	bulletRe  = regexp.MustCompile(`^\s*[•\-\*]\s*`)
	sectionRe = regexp.MustCompile(`(?i)^\s*(summary|experience|projects?|education|technologies|skills|achievements)\s*$`)
)

// Skim evaluates the resume the way a recruiter's 7.4-second scan would.
// name/title/company are the facts that must be findable in the top third.
func Skim(resumeText, name, currentTitle, currentCompany string) SkimReport {
	lines := []string{}
	for _, l := range strings.Split(resumeText, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return SkimReport{}
	}
	topN := len(lines) / 3
	if topN < 6 {
		topN = min(6, len(lines))
	}
	top := norm(strings.Join(lines[:topN], "\n"))
	all := norm(resumeText)

	r := SkimReport{}
	add := func(n string, ok bool, d string) {
		r.Checks = append(r.Checks, SkimCheck{n, ok, d})
	}

	// fixation 1: the name, in the first two lines
	head := norm(strings.Join(lines[:min(2, len(lines))], " "))
	add("name in first 2 lines", strings.Contains(head, norm(name)), name)

	// fixations 2-3: current title and company, inside the top third
	add("current title in top third", strings.Contains(top, norm(currentTitle)), currentTitle)
	add("current company in top third", strings.Contains(top, norm(currentCompany)), currentCompany)

	// fixation 4: dates, so progression is checkable
	add("dates present in top third", dateRe.MatchString(top), "employment dates must be scannable")

	// fixation 5: education findable anywhere
	add("education section present", strings.Contains(all, "b.tech") ||
		strings.Contains(all, "bachelor") || strings.Contains(all, "education"), "")

	// layout: short bullets are fixated on, dense paragraphs are skipped
	var bullets, longBullets, maxLen int
	for _, l := range lines {
		if bulletRe.MatchString(l) {
			bullets++
			n := len(strings.TrimSpace(bulletRe.ReplaceAllString(l, "")))
			if n > maxLen {
				maxLen = n
			}
			if n > 115 {
				longBullets++
			}
		}
	}
	add("bullets are short (<=115 chars)", longBullets == 0,
		fmt.Sprintf("%d bullets, %d over-long, longest %d chars", bullets, longBullets, maxLen))

	// layout: no dense prose blocks (the summary is the one allowed exception)
	dense := 0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if len(t) > 200 && !bulletRe.MatchString(l) && i > 4 {
			dense++
		}
	}
	add("no dense paragraph blocks", dense == 0, fmt.Sprintf("%d dense blocks", dense))

	// layout: clear section headings the eye can anchor to
	sections := 0
	for _, l := range lines {
		if sectionRe.MatchString(l) {
			sections++
		}
	}
	add("clear section headings", sections >= 4, fmt.Sprintf("%d headings found", sections))

	for _, c := range r.Checks {
		if c.OK {
			r.Passed++
		}
	}
	r.Total = len(r.Checks)
	return r
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
