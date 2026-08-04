package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type fallbackAgent struct {
	agents []Agent
}

// NewFallback returns an Agent that tries each agent in order when an
// invocation fails because the current agent process is unavailable.
func NewFallback(agents []Agent) Agent {
	switch len(agents) {
	case 0:
		return nil
	case 1:
		return agents[0]
	default:
		copied := make([]Agent, len(agents))
		copy(copied, agents)
		return &fallbackAgent{agents: copied}
	}
}

func (a *fallbackAgent) Name() string {
	if len(a.agents) == 0 {
		return ""
	}
	return a.agents[0].Name()
}

func (a *fallbackAgent) SupportsSessionResume() bool {
	for _, current := range a.agents {
		if SupportsSessionResume(current) {
			return true
		}
	}
	return false
}

func (a *fallbackAgent) SupportsSessionProvider(provider string) bool {
	for _, current := range a.agents {
		if SupportsSessionProvider(current, provider) {
			return true
		}
	}
	return false
}

func (a *fallbackAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions fails closed over the whole fallback set: the
// wrapper may invoke any member, so it neutralizes the target repo's project
// agent-instruction files only if EVERY member does. A single unverified member
// makes the wrapper report false so the gate is refused rather than risk that
// member running unneutralized.
func (a *fallbackAgent) NeutralizesGateInstructions() bool {
	if len(a.agents) == 0 {
		return false
	}
	for _, current := range a.agents {
		if !NeutralizesGateInstructions(current) {
			return false
		}
	}
	return true
}

func (a *fallbackAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	candidates := a.agents
	if opts.Session != nil && opts.Session.ID != "" && opts.Session.Agent != "" {
		candidates = nil
		for _, current := range a.agents {
			if SupportsSessionProvider(current, opts.Session.Agent) {
				candidates = append(candidates, current)
				break
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("session provider %q is not configured", opts.Session.Agent)
		}
	}
	var lastErr error
	// outages collects the lanes that failed because their provider quota is
	// exhausted. When every candidate ends that way the run must say so, with
	// each lane's reset time, rather than surfacing only the last lane's banner.
	outages := make([]*LaneOutageError, 0, len(candidates))
	for i, current := range candidates {
		currentOpts := opts
		if currentOpts.Session != nil && currentOpts.Session.ID == "" && !SupportsSessionResume(current) {
			currentOpts.Session = nil
			currentOpts.SessionFallback = false
		}
		startedAt := time.Now()
		result, err := current.Run(ctx, currentOpts)
		if !ReportsAgentAttempts(current) {
			emitAgentAttempt(currentOpts, current.Name(), result, err, startedAt, time.Now())
		}
		if err == nil {
			if result != nil && result.Provider == "" {
				result.Provider = current.Name()
			}
			return result, nil
		}
		lastErr = err
		var outage *LaneOutageError
		if errors.As(err, &outage) {
			outages = append(outages, outage)
		}
		if i == len(candidates)-1 {
			if len(outages) == len(candidates) {
				return nil, allLanesExhausted(outages)
			}
			return nil, err
		}
		if !isAgentUnavailableError(err) {
			return nil, err
		}
		next := candidates[i+1]
		if opts.OnChunk != nil {
			opts.OnChunk(fmt.Sprintf("\nagent %s failed (%s); falling back to %s\n", current.Name(), fallbackReason(err), next.Name()))
		}
	}
	return nil, lastErr
}

// allLanesExhausted builds the terminal error for a step that had nowhere left
// to run. It deliberately does not wrap any single lane's outage: the whole
// point is that no one lane is the answer, and a caller matching
// *LaneOutageError would otherwise treat this as one recoverable lane.
func allLanesExhausted(outages []*LaneOutageError) error {
	parts := make([]string, 0, len(outages))
	for _, outage := range outages {
		part := fmt.Sprintf("%s until %s", outage.Lane, outage.Until.Local().Format(resetTimeLayout))
		if outage.Reason != "" {
			part += " (" + truncateRunes(outage.Reason, 120) + ")"
		}
		parts = append(parts, part)
	}
	return fmt.Errorf("every configured agent lane is quota-exhausted, so this step has nowhere to run: %s",
		strings.Join(parts, "; "))
}

func truncateRunes(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max-3]) + "..."
}

func (a *fallbackAgent) Close() error {
	var errs []string
	for _, ag := range a.agents {
		if err := ag.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ag.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close fallback agents: %s", strings.Join(errs, "; "))
	}
	return nil
}

func isAgentUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	// A lane skipped because its provider quota is exhausted never launched a
	// process, so it carries none of the substrings below; it is nonetheless the
	// clearest case of "this lane cannot serve the request, try the next one".
	var outage *LaneOutageError
	if errors.As(err, &outage) {
		return true
	}
	msg := strings.ToLower(err.Error())
	unavailable := []string{
		" start:",
		"start server ",
		" server: start server ",
		" exited:",
		" reported exit code ",
	}
	for _, needle := range unavailable {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func fallbackReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	const max = 160
	if len([]rune(text)) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "..."
}
