package dashboard

import (
	"io/fs"

	"github.com/creamy-ghost/guvna/web"
)

// statusResp mirrors the gateway's /admin/status payload (decode-only).
type statusResp struct {
	Uptime    string      `json:"uptime"`
	Chains    []string    `json:"chains"`
	Steps     []stepStat  `json:"steps"`
	StepsDown int         `json:"steps_down"`
	Keys      []keyStatus `json:"keys"`
	Usage     usageStat   `json:"usage"`
}

type stepStat struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Failures  int    `json:"failures"`
	Down      bool   `json:"down"`
	DownForNs int64  `json:"down_for_ns"`
}

type keyStatus struct {
	Provider  string `json:"provider"`
	Index     int    `json:"index"`
	Env       string `json:"env"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Failures  int    `json:"failures"`
	Requests  int64  `json:"requests"`
	TokensIn  int64  `json:"tokens_in"`
	TokensOut int64  `json:"tokens_out"`
}

type usageStat struct {
	Requests   int             `json:"requests"`
	Streams    int             `json:"streams"`
	Errors     int             `json:"errors"`
	TokensIn   int64           `json:"tokens_in"`
	TokensOut  int64           `json:"tokens_out"`
	ByProvider []providerUsage `json:"by_provider"`
}

type providerUsage struct {
	Provider  string `json:"provider"`
	Requests  int    `json:"requests"`
	Errors    int    `json:"errors"`
	TokensIn  int64  `json:"tokens_in"`
	TokensOut int64  `json:"tokens_out"`
}

// chainList mirrors the gateway's /v1/chains payload (decode-only).
type chainList struct {
	Chains []chainInfo `json:"chains"`
}

type chainInfo struct {
	Name   string      `json:"name"`
	Source string      `json:"source"`
	Steps  []chainStep `json:"steps"`
}

type chainStep struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// View models passed to the templates. Gateway ints stay ints; durations
// are pre-formatted so templates stay logic-free.

// overviewView renders the Overview tab.
type overviewView struct {
	Error     string
	Uptime    string
	Chains    []string
	StepsDown int
	Usage     usageStat
	DownSteps []stepView
}

// keysView renders the Keys tab.
type keysView struct {
	Error string
	Keys  []keyStatus
	Steps []stepView
}

type stepView struct {
	Provider string
	Model    string
	Failures int
	Down     bool
	DownFor  string
}

// logsView renders the Logs tab.
type logsView struct {
	Error string
	N     int
	Lines []string
}

// chainsView renders the Chains tab. FormName/FormSteps preserve the create
// form across validation errors; both are empty on success.
type chainsView struct {
	Error     string
	FormName  string
	FormSteps string
	Chains    []chainInfo
}

// staticFS returns the vendored web/static subtree for the file server.
func staticFS() (fs.FS, error) {
	return fs.Sub(web.FS, "static")
}
