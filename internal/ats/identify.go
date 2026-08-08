package ats

import (
	"net/url"
	"regexp"
	"strings"
)

// Identify maps a public job-posting URL back to the (ats, token, reqID)
// triple the sweep would have recorded for that posting. This is what lets a
// link Harshil pastes enter the same pipeline — and, crucially, hit the same
// (ats, token, req_id) dedupe — as a posting the sweep found on its own.
//
// It recognises only the five tracked board types. Anything else (LinkedIn, a
// company careers page, an aggregator) returns ok=false; the caller supplies
// the JD text itself and the posting is filed under the synthetic 'manual' ATS.
func Identify(raw string) (atsType, token, reqID string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", "", false
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	seg := pathSegments(u.Path)
	q := u.Query()

	switch {
	case strings.HasSuffix(host, "greenhouse.io"):
		// boards / job-boards, both the .io and .eu variants:
		//   .../<token>/jobs/<id>
		if i := indexOf(seg, "jobs"); i > 0 && i+1 < len(seg) {
			if id := leadingDigits(seg[i+1]); id != "" {
				return "greenhouse", seg[i-1], id, true
			}
		}
		// the embedded application form: /embed/job_app?for=<token>&token=<id>
		if id := leadingDigits(q.Get("token")); id != "" && q.Get("for") != "" {
			return "greenhouse", q.Get("for"), id, true
		}
		// a careers page hosting the board inline: /<token>?gh_jid=<id>
		if id := leadingDigits(q.Get("gh_jid")); id != "" && len(seg) > 0 {
			return "greenhouse", seg[0], id, true
		}

	case host == "jobs.lever.co" || host == "jobs.eu.lever.co":
		if len(seg) >= 2 {
			return "lever", seg[0], seg[1], true
		}

	case host == "jobs.ashbyhq.com" || host == "ashbyhq.com":
		if len(seg) >= 2 {
			return "ashby", seg[0], seg[1], true
		}

	case strings.HasSuffix(host, "workable.com"):
		// apply.workable.com/<token>/j/<CODE>/ and <token>.workable.com/j/<CODE>
		if i := indexOf(seg, "j"); i >= 0 && i+1 < len(seg) {
			tok := ""
			switch {
			case i > 0:
				tok = seg[i-1]
			case host != "apply.workable.com":
				tok = strings.TrimSuffix(host, ".workable.com")
			}
			if tok != "" {
				return "workable", tok, strings.ToUpper(seg[i+1]), true
			}
		}

	case strings.HasSuffix(host, "smartrecruiters.com"):
		// jobs.smartrecruiters.com/<token>/<numeric-id>-<slug>
		if len(seg) >= 2 {
			if id := leadingDigits(seg[1]); id != "" {
				return "smartrecruiters", seg[0], id, true
			}
		}
	}
	return "", "", "", false
}

var (
	digitsRe   = regexp.MustCompile(`^\d+`)
	nonSlugRe  = regexp.MustCompile(`[^a-z0-9]+`)
	trackingRe = regexp.MustCompile(`^(utm_.*|gh_src|gclid|fbclid|mc_cid|mc_eid|trk|trackingId|refId|ref|source|src|lever-source.*|li_fat_id)$`)
)

// CanonicalURL strips the fragment and the tracking parameters boards and
// social posts bolt on, so the same posting shared from two places dedupes to
// one entry. Parameters the board actually needs (gh_jid, the Greenhouse embed
// pair) are kept.
func CanonicalURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		if trackingRe.MatchString(k) {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}

// Slug turns a company name or a host into a stable token for the synthetic
// 'manual' ATS: "Acme Corp" and "careers.acme.com" both become usable keys.
func Slug(s string) string {
	s = nonSlugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	return strings.Trim(s, "-")
}

// HostSlug derives a company-ish token from a URL when no company name was given.
func HostSlug(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "unknown"
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	// drop the leading label for the common careers/jobs subdomains, and the TLD
	parts := strings.Split(host, ".")
	for len(parts) > 1 && (parts[0] == "careers" || parts[0] == "jobs" || parts[0] == "apply" || parts[0] == "boards") {
		parts = parts[1:]
	}
	if len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	if s := Slug(strings.Join(parts, "-")); s != "" {
		return s
	}
	return "unknown"
}

func pathSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s = strings.TrimSpace(s); s != "" {
			if dec, err := url.PathUnescape(s); err == nil {
				s = dec
			}
			out = append(out, s)
		}
	}
	return out
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if strings.EqualFold(s, want) {
			return i
		}
	}
	return -1
}

func leadingDigits(s string) string { return digitsRe.FindString(strings.TrimSpace(s)) }
