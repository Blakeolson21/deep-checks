package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// resetTimeLayout renders a lane's recovery time in the operator's local zone,
// with the zone named, because the whole point of the message is telling a
// human when work can resume.
const resetTimeLayout = "2006-01-02 15:04 MST"

// LaneOutageError reports that one agent lane cannot run because the
// provider's quota is exhausted until Until. It wraps the provider failure
// that produced the mark when there is one, so the original banner still
// reaches the step log.
type LaneOutageError struct {
	Lane   string
	Until  time.Time
	Reason string
	cause  error
}

func (e *LaneOutageError) Error() string {
	msg := fmt.Sprintf("agent lane %s is quota-exhausted until %s", e.Lane, e.Until.Local().Format(resetTimeLayout))
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

func (e *LaneOutageError) Unwrap() error { return e.cause }

// IsQuotaOutage reports whether err says provider quota exhaustion is what
// failed the invocation: a single lane's *LaneOutageError (skipped while
// marked, or freshly classified from the provider banner) or the fallback
// wrapper's every-eligible-lane aggregate. Telemetry classification keys on
// this instead of substring-matching error text, which embeds provider banner
// excerpts like "codex exited: ..." and would misfile the outage.
func IsQuotaOutage(err error) bool {
	var lane *LaneOutageError
	if errors.As(err, &lane) {
		return true
	}
	var all *allLanesOutageError
	return errors.As(err, &all)
}

// LaneHealthStore is the slice of lanehealth.Store this package needs, kept as
// an interface so tests and future callers can substitute their own.
type LaneHealthStore interface {
	Outage(lane string) (lanehealth.Outage, bool)
	ClaimProbe(lane string) bool
	Mark(outage lanehealth.Outage) error
	ClearObservedBefore(lane string, startedAt time.Time) error
}

// LaneName returns the key a configured agent name's lane health is recorded
// under: the identity the constructed agent reports from Agent.Name(), which
// for every ACP-driven agent is its target rather than the alias the operator
// configured. Read surfaces must resolve through this so they cannot look up a
// lane the pipeline never writes.
func LaneName(name types.AgentName) string {
	if target, ok := types.ACPTargetFor(name); ok {
		return acpAgentName(target)
	}
	return string(name)
}

// laneHealthAgent skips an invocation entirely while its lane is known to be
// quota-exhausted, and records the outage when a provider quota banner is what
// failed the invocation.
//
// Marking happens here rather than in the fallback wrapper so a single
// configured agent - the default - also fails fast with a reset time instead
// of spawning a process to be told it is out of quota.
//
// A marked lane is not sealed until its reset time: one invocation per
// lanehealth.ProbeInterval is let through, and its success clears the mark. A
// reset the provider stated days out is otherwise trusted from one observation
// with no way to correct it, because the only evidence that could - a
// completed invocation - is what the mark suppresses.
type laneHealthAgent struct {
	Agent
	store LaneHealthStore
	now   func() time.Time
}

// WithLaneHealth wraps a single agent lane with persisted quota-outage
// tracking. A nil store returns the agent unchanged, so demo mode and tests
// that do not care keep the previous behavior exactly.
func WithLaneHealth(a Agent, store LaneHealthStore, now func() time.Time) Agent {
	if a == nil {
		return nil
	}
	if store == nil {
		return a
	}
	if now == nil {
		now = time.Now
	}
	return laneHealthAgent{Agent: a, store: store, now: now}
}

func (l laneHealthAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	lane := l.Agent.Name()
	startedAt := l.now()
	// A lane that reports its own attempts emits them from below this wrapper,
	// carrying the raw provider error, before the outage verdict exists. This
	// wrapper therefore owns attempt fidelity for such a lane. A lane that does
	// not report attempts is reported by the caller from this wrapper's return
	// value, which already carries the verdict.
	reportsAttempts := ReportsAgentAttempts(l.Agent)
	if outage, down := l.store.Outage(lane); down {
		if !l.store.ClaimProbe(lane) {
			err := &LaneOutageError{Lane: lane, Until: outage.Until, Reason: outage.Reason}
			if opts.OnChunk != nil {
				opts.OnChunk("\n" + err.Error() + "\n")
			}
			if reportsAttempts {
				// A skipped lane never reaches the adapter that would report it, so
				// without this the skip leaves no record at all whenever a fallback
				// set moves on to another lane.
				emitAgentAttempt(opts, lane, nil, err, startedAt, l.now())
			}
			return nil, err
		}
		if opts.OnChunk != nil {
			opts.OnChunk(fmt.Sprintf("\nagent lane %s is marked quota-exhausted until %s; sending one probe invocation to check for early recovery\n",
				lane, outage.Until.Local().Format(resetTimeLayout)))
		}
	}

	var relay *attemptRelay
	if opts.OnAttempt != nil && reportsAttempts {
		relay = &attemptRelay{downstream: opts.OnAttempt}
		opts.OnAttempt = relay.capture
	}
	defer relay.release()

	result, err := l.Agent.Run(ctx, opts)
	if err == nil {
		// A completed invocation is direct evidence the lane worked when it was
		// authorized, so any mark that predates it - including one written from a
		// misread banner - is dropped rather than left to expire on its own. A mark
		// a concurrent run observed after this invocation started describes a later
		// state of the account and survives.
		_ = l.store.ClearObservedBefore(lane, startedAt)
		return result, nil
	}
	if ctx.Err() != nil {
		// A cancelled or timed-out run says nothing about the lane's quota, and
		// its partial output may still carry a banner the provider had only
		// warned about. Never park a lane on that evidence.
		return result, err
	}
	var parseErr *OutputParseError
	if errors.As(err, &parseErr) {
		// A schema-parse failure is the one adapter error built from the agent's
		// own final message, and that message can quote a banner verbatim - a
		// review of this very repository does. Classifying it would park a
		// healthy lane, so this failure never reaches the classifier.
		return result, err
	}
	// Only a failed invocation is classified, and its text comes from the
	// provider's stderr and error channel, never from agent-authored output.
	if outage, quota := lanehealth.Classify(lane, err.Error(), l.now()); quota {
		_ = l.store.Mark(outage)
		outageErr := &LaneOutageError{
			Lane:   lane,
			Until:  outage.Until,
			Reason: outage.Reason,
			cause:  err,
		}
		relay.amend(outageErr)
		return nil, outageErr
	}
	return result, err
}

// attemptRelay defers a lane's most recent adapter attempt so the lane wrapper
// can re-report its error as the quota outage it classified only after the
// adapter has already returned. Earlier retry attempts pass through untouched,
// and their order is preserved because attempt N is released the moment
// attempt N+1 arrives. Every method tolerates a nil relay so a lane with no
// attempt reporting needs no branch at the call sites.
type attemptRelay struct {
	downstream func(Attempt)
	held       *Attempt
}

func (r *attemptRelay) capture(attempt Attempt) {
	if r == nil {
		return
	}
	r.release()
	held := attempt
	r.held = &held
}

// amend replaces the held attempt's error with the verdict the wrapper reached,
// so the recorded attempt and the caller's error agree on why the lane failed.
func (r *attemptRelay) amend(err error) {
	if r == nil || r.held == nil {
		return
	}
	r.held.Err = err
}

func (r *attemptRelay) release() {
	if r == nil || r.held == nil {
		return
	}
	attempt := *r.held
	r.held = nil
	r.downstream(attempt)
}

func (l laneHealthAgent) SupportsSessionResume() bool {
	return SupportsSessionResume(l.Agent)
}

func (l laneHealthAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(l.Agent, provider)
}

func (l laneHealthAgent) ReportsAgentAttempts() bool {
	return ReportsAgentAttempts(l.Agent)
}

func (l laneHealthAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(l.Agent)
}
