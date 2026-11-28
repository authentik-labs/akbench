// Package fixtures generates the synthetic test users every benchmark logs
// in as: an authentik blueprint describing a fictional company directory
// (departments, teams, offices, nested access groups, users) and the
// matching k6 credentials (users.json). It's a Go port of
// fixtures/gen-blueprint.py, so the operator can seed fixtures by itself on
// a fresh install (controller.SeedFixtures) instead of requiring
// `make fixtures` first; the Python script is still there for custom runs.
//
// The output has the same shape as the script's - blueprint entries,
// group/user ids, attributes, credentials - but not byte-identical
// content: names come from the embedded lists below, not Faker's.
package fixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// EmailDomain is every generated user's email domain.
const EmailDomain = "authentik.company"

// Credential is one users.json entry: what k6 logs in with.
type Credential struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Fixtures is one generated directory.
type Fixtures struct {
	// Blueprint is the authentik blueprint (YAML) creating every group and
	// user.
	Blueprint []byte
	// Credentials is users.json: every user's username/email/password.
	Credentials []byte
	Users       int
}

// Generate builds a directory of users users. The same seed always
// produces the same directory.
func Generate(users int, seed uint64) (*Fixtures, error) {
	if users < 1 {
		return nil, fmt.Errorf("users must be at least 1, got %d", users)
	}
	g := &generator{
		users:          users,
		rng:            rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		usernames:      map[string]bool{},
		employeeNumber: 10000,
		deptGroups:     map[string]string{},
		teamGroups:     map[string]map[string]string{},
	}
	g.addGroups()
	g.addUsers()

	doc := ordered{
		{"version", 1},
		{"metadata", ordered{
			{"name", fmt.Sprintf("Synthetic company directory (%d users)", users)},
			{"labels", ordered{
				{"blueprints.goauthentik.io/description", "Generated users, groups and nested group memberships for benchmarking."},
			}},
		}},
		{"entries", g.entries},
	}
	var blueprint bytes.Buffer
	enc := yaml.NewEncoder(&blueprint)
	enc.SetIndent(2)
	if err := enc.Encode(toNode(doc)); err != nil {
		return nil, fmt.Errorf("encode blueprint: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	credentials, err := json.MarshalIndent(g.credentials, "", "  ")
	if err != nil {
		return nil, err
	}
	return &Fixtures{Blueprint: blueprint.Bytes(), Credentials: append(credentials, '\n'), Users: users}, nil
}

// AccessRoot is the group every access group nests under. Every user is a
// member of it, but only through a child group (baselineAccess), never
// directly: what the oauth-policies test dependency binds its application
// to, so that admitting them depends on inherited membership.
const AccessRoot = "Application Access"

// baselineAccess is every employee's access, regardless of department.
var baselineAccess = []string{"Google Workspace Users", "Slack Users", "Zoom Users", "VPN Users"}

// accessGroups are (group, parent) pairs, parents first. Nesting mirrors how
// access tiers stack: a member of "AWS Administrators" is also a member of
// "Cloud Platform Access".
var accessGroups = [][2]string{
	{"Application Access", ""},
	{"Google Workspace Users", "Application Access"},
	{"Slack Users", "Application Access"},
	{"Zoom Users", "Application Access"},
	{"Atlassian Users", "Application Access"},
	{"Jira Users", "Atlassian Users"},
	{"Confluence Users", "Atlassian Users"},
	{"GitHub Users", "Application Access"},
	{"GitHub Administrators", "GitHub Users"},
	{"Salesforce Users", "Application Access"},
	{"Salesforce Administrators", "Salesforce Users"},
	{"Zendesk Agents", "Application Access"},
	{"Workday Users", "Application Access"},
	{"NetSuite Users", "Application Access"},
	{"Figma Editors", "Application Access"},
	{"Grafana Viewers", "Application Access"},
	{"Grafana Editors", "Grafana Viewers"},
	{"Infrastructure Access", ""},
	{"VPN Users", "Infrastructure Access"},
	{"VPN Users (Privileged)", "VPN Users"},
	{"Cloud Platform Access", "Infrastructure Access"},
	{"AWS Read Only", "Cloud Platform Access"},
	{"AWS Power Users", "AWS Read Only"},
	{"AWS Administrators", "AWS Power Users"},
	{"Kubernetes Cluster Access", "Infrastructure Access"},
	{"Kubernetes Cluster Admins", "Kubernetes Cluster Access"},
	{"Production Database Read Only", "Infrastructure Access"},
}

var extraAccess = []string{"Figma Editors", "Grafana Editors", "Grafana Viewers", "Confluence Users", "Jira Users"}

type office struct {
	name   string
	weight int
}

var offices = []office{
	{"San Francisco, CA", 22},
	{"New York, NY", 14},
	{"Austin, TX", 10},
	{"London, UK", 12},
	{"Berlin, DE", 10},
	{"Remote (US)", 20},
	{"Remote (EMEA)", 12},
}

type department struct {
	name       string
	weight     int
	costCenter string
	teams      []string
	titles     []string
	access     []string
	teamAccess map[string][]string
}

var departments = []department{
	{
		name: "Engineering", weight: 30, costCenter: "CC-4000",
		teams:  []string{"Platform Engineering", "Frontend Engineering", "Backend Engineering", "Site Reliability Engineering", "Security Engineering", "Quality Engineering"},
		titles: []string{"Software Engineer", "Software Engineer II", "Senior Software Engineer", "Staff Software Engineer", "Principal Engineer"},
		access: []string{"GitHub Users", "Jira Users", "Confluence Users", "Grafana Viewers", "AWS Read Only"},
		teamAccess: map[string][]string{
			"Platform Engineering":         {"GitHub Administrators", "Kubernetes Cluster Access"},
			"Site Reliability Engineering": {"AWS Power Users", "Kubernetes Cluster Admins", "Production Database Read Only"},
			"Security Engineering":         {"VPN Users (Privileged)", "Production Database Read Only"},
		},
	},
	{
		name: "Product", weight: 8, costCenter: "CC-4100",
		teams:  []string{"Product Management", "Product Design", "User Research"},
		titles: []string{"Product Manager", "Senior Product Manager", "Product Designer", "UX Researcher"},
		access: []string{"Jira Users", "Confluence Users", "Figma Editors", "Grafana Viewers"},
	},
	{
		name: "Sales", weight: 16, costCenter: "CC-2000",
		teams:  []string{"Enterprise Sales", "Commercial Sales", "Sales Engineering", "Sales Operations"},
		titles: []string{"Account Executive", "Senior Account Executive", "Sales Development Representative", "Solutions Engineer"},
		access: []string{"Salesforce Users", "NetSuite Users"},
		teamAccess: map[string][]string{
			"Sales Operations":  {"Salesforce Administrators"},
			"Sales Engineering": {"GitHub Users", "Grafana Viewers"},
		},
	},
	{
		name: "Marketing", weight: 8, costCenter: "CC-2100",
		teams:  []string{"Demand Generation", "Product Marketing", "Brand & Communications"},
		titles: []string{"Marketing Manager", "Content Strategist", "Growth Marketer", "Events Manager"},
		access: []string{"Salesforce Users", "Figma Editors"},
	},
	{
		name: "Customer Success", weight: 12, costCenter: "CC-3000",
		teams:      []string{"Technical Support", "Onboarding & Implementation", "Renewals"},
		titles:     []string{"Support Engineer", "Senior Support Engineer", "Customer Success Manager", "Implementation Consultant"},
		access:     []string{"Zendesk Agents", "Salesforce Users", "Jira Users"},
		teamAccess: map[string][]string{"Technical Support": {"Grafana Viewers"}},
	},
	{
		name: "Finance", weight: 6, costCenter: "CC-1000",
		teams:  []string{"Accounting", "Financial Planning & Analysis", "Payroll"},
		titles: []string{"Accountant", "Financial Analyst", "Controller", "Payroll Specialist"},
		access: []string{"NetSuite Users", "Workday Users"},
	},
	{
		name: "People", weight: 5, costCenter: "CC-1100",
		teams:  []string{"Talent Acquisition", "People Operations", "Learning & Development"},
		titles: []string{"Recruiter", "People Partner", "HR Generalist", "Enablement Manager"},
		access: []string{"Workday Users"},
	},
	{
		name: "Legal", weight: 3, costCenter: "CC-1200",
		teams:  []string{"Corporate Legal", "Compliance & Privacy"},
		titles: []string{"Corporate Counsel", "Paralegal", "Compliance Manager", "Privacy Analyst"},
		access: []string{"Workday Users", "Confluence Users"},
	},
	{
		name: "IT", weight: 7, costCenter: "CC-5000",
		teams:  []string{"IT Helpdesk", "Identity & Access Management", "Corporate Infrastructure"},
		titles: []string{"IT Specialist", "Systems Administrator", "IAM Engineer", "Endpoint Engineer"},
		access: []string{"Jira Users", "Workday Users", "AWS Read Only", "VPN Users (Privileged)"},
		teamAccess: map[string][]string{
			"Identity & Access Management": {"authentik Administrators", "AWS Administrators"},
			"Corporate Infrastructure":     {"AWS Power Users"},
		},
	},
	{
		name: "Operations", weight: 5, costCenter: "CC-1300",
		teams:  []string{"Business Operations", "Facilities", "Procurement"},
		titles: []string{"Operations Manager", "Facilities Coordinator", "Procurement Specialist", "Business Analyst"},
		access: []string{"NetSuite Users", "Confluence Users"},
	},
}

var (
	slugInvalid = regexp.MustCompile(`[^a-z0-9]+`)
	slugDashes  = regexp.MustCompile(`-+`)
	nonLetters  = regexp.MustCompile(`[^a-z]`)
)

func slug(value string) string {
	return strings.Trim(slugDashes.ReplaceAllString(slugInvalid.ReplaceAllString(strings.ToLower(value), "-"), "-"), "-")
}

func groupID(name string) string { return "group-" + slug(name) }

type generator struct {
	users          int
	rng            *rand.Rand
	entries        []any
	usernames      map[string]bool
	credentials    []Credential
	employeeNumber int
	deptGroups     map[string]string
	teamGroups     map[string]map[string]string
	officeGroups   []string
	allEmployees   string
	managers       string
	leadership     string
	contractors    string
}

func (g *generator) group(name string, parents []string, attributes ordered, superuser bool) string {
	id := groupID(name)
	var attrs ordered
	if len(parents) > 0 {
		refs := make([]any, len(parents))
		for i, p := range parents {
			refs[i] = keyOf(p)
		}
		attrs = append(attrs, kv{"parents", refs})
	}
	if superuser {
		attrs = append(attrs, kv{"is_superuser", true})
	}
	if attributes != nil {
		attrs = append(attrs, kv{"attributes", attributes})
	}
	entry := ordered{{"model", "authentik_core.group"}, {"id", id}, {"identifiers", ordered{{"name", name}}}}
	if attrs != nil {
		entry = append(entry, kv{"attrs", attrs})
	}
	g.entries = append(g.entries, entry)
	return id
}

func (g *generator) addGroups() {
	g.allEmployees = g.group("All Employees", nil, ordered{{"kind", "company"}}, false)
	g.managers = g.group("Managers", []string{g.allEmployees}, ordered{{"kind", "role"}}, false)
	g.leadership = g.group("Leadership", []string{g.managers}, ordered{{"kind", "role"}}, false)
	g.contractors = g.group("Contractors", nil, ordered{{"kind", "employment-type"}}, false)

	for _, d := range departments {
		g.deptGroups[d.name] = g.group(d.name, []string{g.allEmployees}, ordered{{"kind", "department"}, {"cost_center", d.costCenter}}, false)
	}
	for _, d := range departments {
		g.teamGroups[d.name] = map[string]string{}
		for _, team := range d.teams {
			g.teamGroups[d.name][team] = g.group(team, []string{g.deptGroups[d.name]}, ordered{{"kind", "team"}, {"department", d.name}}, false)
		}
	}

	officesRoot := g.group("Offices", nil, ordered{{"kind", "location"}}, false)
	for _, o := range offices {
		g.officeGroups = append(g.officeGroups, g.group(o.name, []string{officesRoot}, ordered{{"kind", "office"}, {"city", o.name}}, false))
	}

	for _, ag := range accessGroups {
		var parents []string
		if ag[1] != "" {
			parents = []string{groupID(ag[1])}
		}
		g.group(ag[0], parents, ordered{{"kind", "access"}}, false)
	}
	g.group("authentik Administrators", nil, ordered{{"kind", "access"}}, true)
}

// departmentSizes splits users across departments by weight, handing the
// rounding remainder to the largest departments so the total is exact.
func (g *generator) departmentSizes() []int {
	total := 0
	for _, d := range departments {
		total += d.weight
	}
	sizes := make([]int, len(departments))
	assigned := 0
	for i, d := range departments {
		sizes[i] = g.users * d.weight / total
		assigned += sizes[i]
	}
	largest := make([]int, len(departments))
	for i := range largest {
		largest[i] = i
	}
	slices.SortStableFunc(largest, func(a, b int) int { return departments[b].weight - departments[a].weight })
	for i := 0; i < g.users-assigned; i++ {
		sizes[largest[i%len(largest)]]++
	}
	return sizes
}

func (g *generator) uniqueUsername(first, last string) string {
	base := nonLetters.ReplaceAllString(strings.ToLower(first[:1]+last), "")
	username := base
	for suffix := 2; g.usernames[username]; suffix++ {
		username = fmt.Sprintf("%s%d", base, suffix)
	}
	g.usernames[username] = true
	return username
}

func (g *generator) weighted(weights []int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	n := g.rng.IntN(total)
	for i, w := range weights {
		if n < w {
			return i
		}
		n -= w
	}
	return len(weights) - 1
}

func (g *generator) accessGroupIDs(d department, team string) []string {
	names := slices.Concat(baselineAccess, d.access, d.teamAccess[team])
	extra := slices.Clone(extraAccess)
	g.rng.Shuffle(len(extra), func(i, j int) { extra[i], extra[j] = extra[j], extra[i] })
	names = append(names, extra[:g.weighted([]int{70, 22, 8})]...)
	ids := make([]string, len(names))
	for i, n := range names {
		ids[i] = groupID(n)
	}
	return ids
}

func (g *generator) addUsers() {
	for i, size := range g.departmentSizes() {
		if size == 0 {
			continue
		}
		d := departments[i]
		director := g.addUser(d, "", "Director, "+d.name, []string{g.managers, g.leadership}, "")
		leads := map[string]string{}
		for index := 0; index < size-1; index++ {
			team := d.teams[index%len(d.teams)]
			if _, ok := leads[team]; !ok {
				leads[team] = g.addUser(d, team, "Manager, "+team, []string{g.managers}, director)
				continue
			}
			g.addUser(d, team, d.titles[g.rng.IntN(len(d.titles))], nil, leads[team])
		}
	}
}

func (g *generator) addUser(d department, team, title string, extraGroups []string, manager string) string {
	first, last := firstNames[g.rng.IntN(len(firstNames))], lastNames[g.rng.IntN(len(lastNames))]
	username := g.uniqueUsername(first, last)
	email := username + "@" + EmailDomain
	password := g.password(24)
	g.employeeNumber++
	g.credentials = append(g.credentials, Credential{Username: username, Email: email, Password: password})

	// Only rank-and-file members can be contractors.
	contractor := len(extraGroups) == 0 && g.rng.Float64() < 0.06
	home := g.deptGroups[d.name]
	if team != "" {
		home = g.teamGroups[d.name][team]
	}
	officeWeights := make([]int, len(offices))
	for i, o := range offices {
		officeWeights[i] = o.weight
	}
	groups := []string{home, g.officeGroups[g.weighted(officeWeights)]}
	groups = append(groups, g.accessGroupIDs(d, team)...)
	groups = append(groups, extraGroups...)
	if contractor {
		groups = append(groups, g.contractors)
	}

	kind, prefix := "internal", "E"
	if contractor {
		kind, prefix = "external", "C"
	}
	attributes := ordered{
		{"title", title},
		{"department", d.name},
		{"employee_number", fmt.Sprintf("%s%d", prefix, g.employeeNumber)},
		{"cost_center", d.costCenter},
	}
	if team != "" {
		attributes = append(attributes, kv{"team", team})
	}
	if manager != "" {
		attributes = append(attributes, kv{"manager", manager})
	}

	seen := map[string]bool{}
	var groupRefs []any
	for _, id := range groups {
		if !seen[id] {
			seen[id] = true
			groupRefs = append(groupRefs, keyOf(id))
		}
	}
	g.entries = append(g.entries, ordered{
		{"model", "authentik_core.user"},
		{"id", "user-" + username},
		{"identifiers", ordered{{"username", username}}},
		{"attrs", ordered{
			{"name", first + " " + last},
			{"email", email},
			{"password", password},
			{"is_active", true},
			{"type", kind},
			{"path", "users/" + slug(d.name)},
			{"attributes", attributes},
			{"groups", groupRefs},
		}},
	})
	return email
}

const (
	lowerChars   = "abcdefghijklmnopqrstuvwxyz"
	upperChars   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digitChars   = "0123456789"
	specialChars = "!@#$%^&*()_+"
)

// password is length characters with at least one of each class, like
// Faker's password(special_chars=True, digits=True, upper_case=True).
func (g *generator) password(length int) string {
	pick := func(set string) byte { return set[g.rng.IntN(len(set))] }
	all := lowerChars + upperChars + digitChars + specialChars
	out := []byte{pick(lowerChars), pick(upperChars), pick(digitChars), pick(specialChars)}
	for len(out) < length {
		out = append(out, pick(all))
	}
	g.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return string(out)
}

// -- YAML: ordered mappings and authentik's !KeyOf tag, which the plain
// map-based encoders can't express.

type kv struct {
	k string
	v any
}

type ordered []kv

// keyOf is a group reference, encoded as authentik's `!KeyOf <id>` tag.
type keyOf string

func toNode(v any) *yaml.Node {
	switch v := v.(type) {
	case ordered:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, e := range v {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: e.k}, toNode(e.v))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range v {
			n.Content = append(n.Content, toNode(item))
		}
		return n
	case keyOf:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!KeyOf", Value: string(v)}
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(v)}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(v)}
	default:
		panic(fmt.Sprintf("fixtures: unsupported YAML value %T", v))
	}
}
