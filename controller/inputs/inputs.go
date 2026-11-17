// Package inputs fetches a lease's input files before the lease is
// acknowledged. Both controllers use it — the Kubernetes one and the Docker
// host — because the rule is the runtime contract's and not either runtime's:
// nothing starts before the acknowledgement, and a run the controller has
// taken finishes even while the backend is down. Fetching first is what keeps
// both true for a run that has files: by the time the backend hears "taken",
// every byte the pod will ask for is on this controller's own volume.
//
// The outcome of a fetch is one of three, and each maps to one thing the lease
// loop does:
//
//   - nil, nil: every input is spooled and verified; materialise and ack.
//   - a rejection: the lease cannot be run here as it stands — it breaks the
//     contract's ceilings, an input is gone for good, or the bytes are not the
//     ones the manifest names. A negative ack, which the backend answers by
//     raising the epoch and offering the work elsewhere.
//   - an error: nothing is known — the backend is unreachable, or the fetch
//     ran out of time. No ack at all; the ack deadline expires and the backend
//     reassigns, which is the outcome an unreachable backend already gets.
//
// Input names and bytes are what a person sent. Nothing here logs either, or
// puts one into an error or a rejection: an input is named by its position in
// the manifest.
package inputs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/spool"
)

// API is the part of the Cluster API a fetch uses.
type API interface {
	LeaseInput(ctx context.Context, runID runv1.ULID, epoch int64, file string) (*clusterapi.InputBody, error)
}

// DefaultRequestTimeout bounds one input's transfer. At the ceiling of 100 MiB
// for a whole lease it allows roughly 15 Mbit/s for the largest single file;
// the lease's own budget, below, is the bound that actually protects the ack
// deadline.
const DefaultRequestTimeout = 60 * time.Second

// Fetcher fetches inputs into the spool.
type Fetcher struct {
	API   API
	Spool *spool.Spool
	// RequestTimeout bounds each input's request; zero means
	// DefaultRequestTimeout. The context passed to Fetch bounds the whole.
	RequestTimeout time.Duration
	Log            *slog.Logger
}

func (f *Fetcher) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

// Budget is how long a lease loop gives a fetch: two thirds of the ack
// timeout, so that materialising and acknowledging still fit inside the
// deadline the backend measures from the moment it issued the lease.
func Budget(ackTimeoutSeconds int32) time.Duration {
	if ackTimeoutSeconds <= 0 {
		ackTimeoutSeconds = clusterv1.DefaultTimings().AckTimeoutSeconds
	}
	return time.Duration(ackTimeoutSeconds) * time.Second * 2 / 3
}

// Validate checks a lease's inputs against the contract without touching the
// network or the disk: the count and total ceilings, every key against the
// allow-list, no key twice, a digest that is 64 lowercase hex characters and
// a size that is not negative. A lease that fails is refused with InvalidSpec
// naming the inputs field.
func Validate(in []runv1.InputRef) *clusterv1.AckRejection {
	refuse := func(format string, args ...any) *clusterv1.AckRejection {
		return &clusterv1.AckRejection{
			Code:    clusterv1.RejectInvalidSpec,
			Message: fmt.Sprintf(format, args...),
			Fields:  []string{"inputs"},
		}
	}
	if len(in) > clusterv1.MaxInputsPerLease {
		return refuse("the lease carries %d inputs and a controller fetches at most %d",
			len(in), clusterv1.MaxInputsPerLease)
	}
	seen := make(map[string]bool, len(in))
	var total int64
	for i, ref := range in {
		n := i + 1
		if _, err := runv1.InputKey(ref.Key); err != nil {
			return refuse("input %d has a key outside the contract's layout", n)
		}
		if seen[ref.Key] {
			return refuse("input %d repeats the key of an earlier input", n)
		}
		seen[ref.Key] = true
		if !isDigest(ref.SHA256) {
			return refuse("input %d does not carry a lowercase hex SHA-256 digest", n)
		}
		if ref.SizeBytes < 0 {
			return refuse("input %d has a negative size", n)
		}
		total += ref.SizeBytes
		if total > clusterv1.MaxInputBytesPerLease {
			return refuse("the lease's inputs come to more than %d bytes, the most a controller fetches",
				int64(clusterv1.MaxInputBytesPerLease))
		}
	}
	return nil
}

func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Fetch makes every input of the lease durable in the spool, verified against
// the manifest. An input already spooled and intact — a controller that
// restarted between fetching and acknowledging — is not fetched again; one
// that is corrupt or short is.
//
// The context bounds the whole fetch; the caller derives it from the ack
// deadline (see Budget).
func (f *Fetcher) Fetch(ctx context.Context, lease clusterv1.Lease) (*clusterv1.AckRejection, error) {
	if len(lease.Inputs) == 0 {
		return nil, nil
	}
	if rejection := Validate(lease.Inputs); rejection != nil {
		return rejection, nil
	}
	if f.Spool == nil || f.API == nil {
		// A controller wired without a spool cannot hold what the pod will ask
		// for. Refused rather than left unacknowledged: no repeat of this lease
		// to this controller will go differently.
		return &clusterv1.AckRejection{
			Code:    clusterv1.RejectMaterializationFailed,
			Message: "this controller has nowhere to keep a run's input files",
		}, nil
	}

	fetched := 0
	for i, ref := range lease.Inputs {
		n := i + 1
		ok, err := f.Spool.InputMatches(lease.RunID, ref)
		if err != nil {
			return nil, fmt.Errorf("inputs: check input %d of %s: %w", n, lease.RunID, err)
		}
		if ok {
			continue
		}
		rejection, err := f.fetchOne(ctx, lease, n, ref)
		if rejection != nil || err != nil {
			return rejection, err
		}
		fetched++
	}
	f.log().Info("the lease's inputs are spooled",
		"runID", lease.RunID, "epoch", lease.Epoch, "inputs", len(lease.Inputs), "fetched", fetched)
	return nil, nil
}

// Confirm is the check a lease loop makes after materialising and before
// acknowledging: every input is still where Fetch put it. Something may have
// removed a run's inputs in between — the old epoch of the same run being
// collected, a sweep — and an acknowledgement for a run whose files are gone
// would be a promise the pod finds broken. Inputs that went missing are
// fetched again.
func (f *Fetcher) Confirm(ctx context.Context, lease clusterv1.Lease) (*clusterv1.AckRejection, error) {
	if len(lease.Inputs) == 0 {
		return nil, nil
	}
	if f.Spool != nil {
		all := true
		for _, ref := range lease.Inputs {
			if !f.Spool.HasInput(lease.RunID, ref) {
				all = false
				break
			}
		}
		if all {
			return nil, nil
		}
	}
	return f.Fetch(ctx, lease)
}

// fetchOne transfers one input. The three outcomes are the package comment's.
func (f *Fetcher) fetchOne(ctx context.Context, lease clusterv1.Lease, n int, ref runv1.InputRef) (*clusterv1.AckRejection, error) {
	timeout := f.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := f.API.LeaseInput(reqCtx, lease.RunID, lease.Epoch, ref.File())
	if err != nil {
		if clusterapi.ActionOf(err) == clusterv1.ActionFatal {
			// InputNotFound, above all: the manifest names a file the backend
			// cannot produce, and no repeat will. Starting the run without it
			// would have the agent told about a file it cannot open.
			code := clusterv1.CodeInternal
			if p, ok := clusterapi.ProblemOf(err); ok {
				code = p.Code
			}
			return &clusterv1.AckRejection{
				Code:    clusterv1.RejectMaterializationFailed,
				Message: fmt.Sprintf("input %d of %d could not be fetched from the control plane: %s", n, len(lease.Inputs), code),
				Fields:  []string{"inputs"},
			}, nil
		}
		// Abandon, retry, backoff, a transport failure: nothing to refuse and
		// nothing to acknowledge. Abandon in particular means the work moved on
		// and an ack would be refused anyway.
		return nil, fmt.Errorf("inputs: fetch input %d of %s: %w", n, lease.RunID, err)
	}
	defer func() { _ = body.Close() }()

	if body.SizeBytes >= 0 && body.SizeBytes != ref.SizeBytes {
		// Refused on the header, before the transfer is paid for. The bytes
		// are not the ones the lease names, and fetching them to prove it would
		// spend the ack deadline on a file that will be thrown away.
		return mismatch(lease, n), nil
	}

	err = f.Spool.WriteInput(lease.RunID, ref, body)
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, spool.ErrInputMismatch) && reqCtx.Err() == nil:
		// The transfer completed and the bytes are wrong: a backend whose
		// store and manifest disagree. Fetching again yields the same bytes.
		// A body that failed to arrive is never this case — WriteInput reports
		// a read error as a write failure, not as a mismatch.
		return mismatch(lease, n), nil
	default:
		// A cut transfer or a local disk problem. Both are worth another lease.
		return nil, fmt.Errorf("inputs: spool input %d of %s: %w", n, lease.RunID, err)
	}
}

func mismatch(lease clusterv1.Lease, n int) *clusterv1.AckRejection {
	return &clusterv1.AckRejection{
		Code: clusterv1.RejectMaterializationFailed,
		Message: fmt.Sprintf("input %d of %d does not match the size and digest the lease names",
			n, len(lease.Inputs)),
		Fields: []string{"inputs"},
	}
}
