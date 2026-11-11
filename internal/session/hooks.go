package session

import (
	"fmt"

	"github.com/tomasz-tomczyk/crit/internal/vcs"
)

// CLIReviewConfig holds parsed review CLI flags for RunReview.
type CLIReviewConfig struct {
	Files              []string
	Focus              *Focus
	OutputDir          string
	PlanDir            string
	NoOpen             bool
	OpenCmd            string
	Quiet              bool
	NoIntegrationCheck bool
	VCSOverride        string
	Perforce           *vcs.P4VCS
	BaseBranch         string
	IgnorePatterns     []string
	SessionID          string
}

var (
	ResolveServerConfigFn func(args []string) (*CLIReviewConfig, error)
	PreflightCheckFn      func(sc *CLIReviewConfig) string
)

// FocusKeyArgs returns daemon session key file args for PR/range focus.
func FocusKeyArgs(sc *CLIReviewConfig) []string {
	if sc != nil && sc.Perforce != nil {
		return sc.Perforce.SessionKeyArgs()
	}
	if sc == nil || sc.Focus == nil || sc.Focus.Kind != FocusRange {
		if sc == nil {
			return nil
		}
		return sc.Files
	}
	if sc.Focus.ChangeNumber > 0 {
		if sc.Focus.Forge == "gitlab" {
			return []string{MRFocusKey(sc.Focus.ChangeNumber, sc.Focus.RemoteBaseProject, sc.Focus.RemoteHost)}
		}
		return []string{PRFocusKey(sc.Focus.ChangeNumber, sc.Focus.RemoteBaseProject, sc.Focus.RemoteHost)}
	}
	return []string{fmt.Sprintf("range:%s..%s", sc.Focus.BaseSHA, sc.Focus.HeadSHA)}
}

func (sc *CLIReviewConfig) ReviewBranch() string {
	if sc.Perforce != nil {
		return sc.Perforce.CurrentBranch()
	}
	if v := vcs.DetectVCS(sc.VCSOverride); v != nil {
		return v.CurrentBranch()
	}
	return ""
}
