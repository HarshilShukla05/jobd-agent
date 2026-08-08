package ats

import "testing"

func TestIdentify(t *testing.T) {
	cases := []struct {
		url               string
		ats, token, reqID string
		ok                bool
	}{
		{"https://job-boards.greenhouse.io/crunchyroll/jobs/6696781",
			"greenhouse", "crunchyroll", "6696781", true},
		{"https://boards.greenhouse.io/rubrik/jobs/8050714?gh_src=abc123",
			"greenhouse", "rubrik", "8050714", true},
		{"https://boards.eu.greenhouse.io/acme/jobs/4012345678#app",
			"greenhouse", "acme", "4012345678", true},
		{"https://boards.greenhouse.io/embed/job_app?for=acme&token=99887766",
			"greenhouse", "acme", "99887766", true},
		{"https://careers.acme.com/roles?gh_jid=1234567", "", "", "", false},
		{"https://jobs.lever.co/kiwi/6a1f0e2c-1111-4b0e-9d3a-000000000000",
			"lever", "kiwi", "6a1f0e2c-1111-4b0e-9d3a-000000000000", true},
		{"https://jobs.lever.co/kiwi/6a1f0e2c-1111-4b0e-9d3a-000000000000/apply",
			"lever", "kiwi", "6a1f0e2c-1111-4b0e-9d3a-000000000000", true},
		{"https://jobs.ashbyhq.com/zoca/8b7c6d5e-2222-4a1b-8c9d-111111111111",
			"ashby", "zoca", "8b7c6d5e-2222-4a1b-8c9d-111111111111", true},
		{"https://apply.workable.com/hevodata/j/19259ABCDE/",
			"workable", "hevodata", "19259ABCDE", true},
		{"https://netomi.workable.com/j/abc123def4",
			"workable", "netomi", "ABC123DEF4", true},
		{"https://jobs.smartrecruiters.com/Target/743999999999-software-engineer",
			"smartrecruiters", "Target", "743999999999", true},
		// not a tracked board — caller must supply the JD text
		{"https://www.linkedin.com/jobs/view/4123456789/", "", "", "", false},
		{"https://careers.google.com/jobs/results/12345", "", "", "", false},
		{"not a url", "", "", "", false},
		// board index pages carry no posting
		{"https://jobs.lever.co/kiwi", "", "", "", false},
		{"https://jobs.ashbyhq.com/zoca", "", "", "", false},
	}
	for _, c := range cases {
		a, tok, req, ok := Identify(c.url)
		if a != c.ats || tok != c.token || req != c.reqID || ok != c.ok {
			t.Errorf("Identify(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				c.url, a, tok, req, ok, c.ats, c.token, c.reqID, c.ok)
		}
	}
}

func TestCanonicalURL(t *testing.T) {
	cases := [][2]string{
		{"https://job-boards.greenhouse.io/acme/jobs/123?gh_src=x&utm_source=linkedin#top",
			"https://job-boards.greenhouse.io/acme/jobs/123"},
		{"https://boards.greenhouse.io/acme/jobs/123/", "https://boards.greenhouse.io/acme/jobs/123"},
		{"https://careers.acme.com/roles?gh_jid=99&ref=twitter",
			"https://careers.acme.com/roles?gh_jid=99"},
		{"https://www.linkedin.com/jobs/view/412/?trk=feed&refId=z",
			"https://www.linkedin.com/jobs/view/412"},
	}
	for _, c := range cases {
		if got := CanonicalURL(c[0]); got != c[1] {
			t.Errorf("CanonicalURL(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestHostSlug(t *testing.T) {
	cases := [][2]string{
		{"https://careers.zetwerk.com/job/123", "zetwerk"},
		{"https://www.linkedin.com/jobs/view/1", "linkedin"},
		{"https://jobs.acme.co.uk/x", "acme-co"},
		{"garbage", "unknown"},
	}
	for _, c := range cases {
		if got := HostSlug(c[0]); got != c[1] {
			t.Errorf("HostSlug(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}
