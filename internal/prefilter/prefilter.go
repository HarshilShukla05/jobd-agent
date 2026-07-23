// Package prefilter is the $0 deterministic gate that kills obviously
// ineligible postings on title/location alone, so the LLM gate only ever
// sees plausible candidates. Rules encode v1's learnings: obvious senior
// titles are an obvious exclude; ambiguous titles PASS here — the JD body
// gate is the ground truth (titles lie in both directions).
package prefilter

import (
	"regexp"
	"strings"
)

var (
	// hard excludes: seniority or non-engineering function in the title
	excludeRe = regexp.MustCompile(`(?i)\b(senior|staff|principal|lead|architect|director|head|manager|sr\.?|vp|chief|iii|iv|intern(ship)?|counsel|attorney|sales|marketing|recruit(er|ing)|talent|people ops|hr\b|human resources|finance|account(ant|ing| executive| manager)|legal|design(er)?|copywriter|writer|support|success|analyst|scientist|consultant|clinical|nurse|physician|teacher|tutor|customer|client[- ]facing|presales|pre-sales|solutions? engineer|sdet|test(er|ing)?|quality assurance|qa\b|distinguished|audio|video)\b`)

	// stack-mismatch titles: not Harshil's stack, reject before paying for a gate
	stackExcludeRe = regexp.MustCompile(`(?i)\.net|\bc#|\bphp\b|wordpress|salesforce|\bsap\b|abap|drupal|sharepoint|servicenow|dynamics|mainframe|cobol|\bios\b|android|flutter|react native|\bfrontend\b|front[- ]end|\bui\b|\bux\b|unity|unreal|embedded|firmware|verilog|\bfpga\b|alteryx|axiom|connex|\bbods\b|informatica|tableau|power ?bi|\bbi\b|sharepoint|pega|mulesoft|talend|snowflake|databricks|\bdata ?bricks\b|\betl\b|\bodoo\b|magento|shopify|\bsfdc\b`)

	// hardware / semiconductor / non-software engineering
	hardwareRe = regexp.MustCompile(`(?i)\b(analog|layout|dft|rtl|asic|vlsi|silicon|semiconductor|characteri[sz]ation|physical design|circuit|hardware|mechanical|electrical|civil|chemical|process|manufactur\w*|warehouse|field|network|data ?cent(er|re)|noc|desktop|helpdesk|technician)\b`)

	// Roles adjacent to, but not, Harshil's target. NOTE: "AI Engineer" is
	// deliberately NOT excluded — his LLM/RAG/agentic pipeline work at Clarity
	// makes those a strong fit. Modeling-heavy ML/CV roles are excluded.
	roleExcludeRe = regexp.MustCompile(`(?i)\b(data engineer|data engineering|machine learning|\bml\b|\bmlops\b|nlp|deep learning|computer vision|research(er)?|robotics|blockchain|web3|solidity|game|graphics|security|infosec|penetration|compliance)\b`)

	// the title must look like an engineering IC role at all
	includeRe = regexp.MustCompile(`(?i)\b(engineer|developer|sde|swe|programmer)\b|\b(backend|back-end|full[- ]?stack|platform|sre|devops|infra(structure)?)\b`)

	indiaRe = regexp.MustCompile(`(?i)\b(india|bengaluru|bangalore|hyderabad|pune|gurugram|gurgaon|mumbai|chennai|noida|delhi|ncr|kolkata|ahmedabad|jaipur|indore|kochi|thiruvananthapuram|trivandrum|coimbatore|nagpur|bhopal|vadodara)\b`)

	// explicit non-India markers — these beat a bare "remote"
	nonIndiaRe = regexp.MustCompile(`(?i)\b(u\.?s\.?a?\b|united states|america|canada|mexico|brazil|argentina|colombia|ecuador|peru|chile|uk\b|united kingdom|england|london|ireland|germany|berlin|france|paris|spain|madrid|portugal|lisbon|netherlands|amsterdam|poland|warsaw|romania|ukraine|israel|tel aviv|uae|dubai|singapore|japan|tokyo|china|beijing|shanghai|korea|australia|sydney|melbourne|new zealand|philippines|manila|indonesia|jakarta|vietnam|thailand|malaysia|kuala lumpur|nigeria|kenya|south africa|egypt|turkey|emea|latam|apac|anywhere in europe|europe\b)\b`)

	// Remote is acceptable ANYWHERE (Harshil's call) — only on-site roles
	// outside India are rejected. Work-authorization is the JD gate's problem.
	remoteRe = regexp.MustCompile(`(?i)\b(remote|work from home|wfh|anywhere|distributed|virtual)\b`)
)

// Check returns keep=true or a short rejection note.
func Check(title, location string) (keep bool, note string) {
	t := strings.TrimSpace(title)
	if excludeRe.MatchString(t) {
		return false, "prefilter: title excluded (" + excludeRe.FindString(t) + ")"
	}
	if stackExcludeRe.MatchString(t) {
		return false, "prefilter: stack mismatch in title (" + stackExcludeRe.FindString(t) + ")"
	}
	if hardwareRe.MatchString(t) {
		return false, "prefilter: not a software role (" + hardwareRe.FindString(t) + ")"
	}
	if roleExcludeRe.MatchString(t) {
		return false, "prefilter: off-target role type (" + roleExcludeRe.FindString(t) + ")"
	}
	if !includeRe.MatchString(t) {
		return false, "prefilter: not an engineering IC title"
	}
	// an on-site country in the TITLE disqualifies too — unless it's remote
	if nonIndiaRe.MatchString(t) && !indiaRe.MatchString(t) && !remoteRe.MatchString(t) {
		return false, "prefilter: non-India on-site in title (" + nonIndiaRe.FindString(t) + ")"
	}

	loc := strings.TrimSpace(location)
	switch {
	case loc == "":
		return true, "" // boards often omit it; the JD gate verifies
	case remoteRe.MatchString(loc):
		return true, "" // remote anywhere is fine
	case indiaRe.MatchString(loc):
		return true, ""
	case nonIndiaRe.MatchString(loc):
		return false, "prefilter: on-site outside India (" + loc + ")"
	}
	return false, "prefilter: location not India and not remote (" + loc + ")"
}
