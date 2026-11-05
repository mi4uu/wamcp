// Package about describes this build and links to the project, including
// pre-filled GitHub issue forms. Nothing from the archive goes into a link.
package about

import (
	"net/url"
	"runtime"
)

const Repo = "https://github.com/mi4uu/whatsupd"

// Info is what the web client's About section and `whatsupd bug` show.
type Info struct {
	Version     string `json:"version"`
	Go          string `json:"go"`
	Platform    string `json:"platform"`
	Setup       string `json:"setup"`
	Repo        string `json:"repo"`
	Docs        string `json:"docs"`
	Releases    string `json:"releases"`
	BugURL      string `json:"bug_url"`
	FeatureURL  string `json:"feature_url"`
	SecurityURL string `json:"security_url"`
}

// New describes this build. setup is a short, non-personal summary of how
// the daemon runs, for example "MCP read-only, OAuth off".
func New(version, setup string) Info {
	i := Info{
		Version:     version,
		Go:          runtime.Version(),
		Platform:    runtime.GOOS + "/" + runtime.GOARCH,
		Setup:       setup,
		Repo:        Repo,
		Docs:        Repo + "#readme",
		Releases:    Repo + "/releases",
		FeatureURL:  issueURL("feature_request.yml", nil),
		SecurityURL: Repo + "/security/advisories/new",
	}
	i.BugURL = issueURL("bug_report.yml", url.Values{
		"version":  {version},
		"platform": {i.Platform + ", " + i.Go},
		"setup":    {setup},
	})
	return i
}

// issueURL opens a GitHub issue form; query keys matching field ids pre-fill
// those fields.
func issueURL(template string, fields url.Values) string {
	q := url.Values{"template": {template}}
	for k, v := range fields {
		q[k] = v
	}
	return Repo + "/issues/new?" + q.Encode()
}
