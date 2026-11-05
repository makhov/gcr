package kubemigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// Transient says whether err, from an API call, is the API server's own
// trouble rather than an answer: it restarting, overloaded, or the
// connection to it dropping. Such a call can be made again.
func Transient(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) || apierrors.IsUnexpectedServerError(err)
}

// poll is wait.PollUntilContextTimeout, immediate, whose condition polls
// again on a Transient error rather than ending the wait: a migration
// whose Job ran on cannot be given up for the API server pausing as it
// watched it. A wait that runs out says the last such error.
func poll(ctx context.Context, interval, timeout time.Duration, cond wait.ConditionWithContextFunc) error {
	var last error
	err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		done, err := cond(ctx)
		if err != nil && Transient(err) {
			last = err
			return false, nil
		}
		return done, err
	})
	if err != nil && last != nil && wait.Interrupted(err) {
		err = fmt.Errorf("%w (the API server last said: %v)", err, last)
	}
	return err
}

// afterMove backs off over a minute or so: the steps after the pod's
// processes moved must not fail on the API server's trouble, or the pod
// is left half handed over.
var afterMove = wait.Backoff{Duration: 500 * time.Millisecond, Factor: 1.5, Jitter: 0.1, Steps: 12, Cap: 10 * time.Second}

// again calls fn until it succeeds or fails other than Transiently.
func again(fn func() error) error {
	return retry.OnError(afterMove, Transient, fn)
}
