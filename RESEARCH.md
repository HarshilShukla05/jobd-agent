# How resume screening actually works (sourced, 2026-08)

Researched before building the scoring layer, because the common "beat the ATS
keyword score" advice turned out to be mostly wrong for the boards this pipeline
targets (Greenhouse, Lever, Ashby).

## 1. Keyword auto-rejection is largely a myth on these boards

Greenhouse does **not** score resumes by keyword density and auto-bin the bottom
half. Rejections there are made by a human. The only automatic rejection is from
**knockout questions** — required application-form questions (years of
experience, work authorization, relocation, salary expectations) that reject
without human review.

**Implication for this pipeline:** the highest-risk step is not the resume, it is
the *application form answers*. A wrong knockout answer is an instant, invisible
reject. `references/profile.md` accuracy matters more than resume wording.

## 2. Keywords still matter — for retrieval, not scoring

Recruiters do not scroll the pile; they **search** it with boolean queries built
from the JD (languages, tools, titles). A resume missing the searched term never
surfaces. So keyword coverage is about **being findable**, and the fix is to
carry the JD's own vocabulary wherever it is truthful.

This is what `atsmatch.Score()` measures. It is a searchability estimate — not a
prediction of rejection.

## 3. AI screening is now mainstream

~82% of firms use AI to help sift resumes; the better tools assess contextual fit
rather than keyword counts. This is the layer `llm.ScreenResume()` simulates.

## 4. The decision is a 7.4-second human skim

Ladders' eye-tracking work found recruiters spend ~7.4s on the initial screen and
fixate on six things:

1. Name
2. Current title
3. Current company
4. Previous title / company
5. Start and end dates of both (they check progression)
6. Education

Layout findings: simple layouts with clear section headings win; **short bullets
attract fixation, dense paragraphs are skipped entirely.**

**Implication:** the top third of page one does almost all the work, and long
bullets are wasted ink. This is what `atsmatch.Skim()` checks.

## What this changed in the pipeline

- Reframed the keyword score as *searchability*, not a pass/fail gate.
- Added `Skim()` for the six fixation points and the layout rules.
- Bullet length cap enforced at <=115 chars (the bank already targets <=105).
- The apply skill now treats **knockout questions as the highest-risk step**, and
  requires answers to come from `references/profile.md` only.
- Kept summary/section ordering that front-loads title, company and dates.

## Sources

- Jobscan — Greenhouse ATS: how it actually handles resumes:
  https://www.jobscan.co/blog/greenhouse-ats-what-job-seekers-need-to-know/
- Greenhouse support — searching candidates with boolean queries:
  https://support.greenhouse.io/hc/en-us/articles/202360199-Search-candidates-using-Boolean-queries
- HR Dive — eye-tracking study, recruiters look at resumes for ~7 seconds:
  https://www.hrdive.com/news/eye-tracking-study-shows-recruiters-look-at-resumes-for-7-seconds/541582/
- Ladders — updated recruiter eye-tracking study (7.4s, six fixation points):
  https://www.prnewswire.com/news-releases/ladders-updates-popular-recruiter-eye-tracking-study-with-new-key-insights-on-how-job-seekers-can-improve-their-resumes-300744217.html
- Jobscan — what an ATS is and what it does:
  https://www.jobscan.co/blog/8-things-you-need-to-know-about-applicant-tracking-systems/
