package cmdsignals

import (
	"context"
	"os"

	"github.com/ngicks/go-common/atomicsignal"
)

// SignalReceivedError aliases [atomicsignal.SignalReceivedError], the
// cancellation cause [NotifyContext] uses, so references stay stable as
// `cmdsignals.SignalReceivedError` without importing atomicsignal.
type SignalReceivedError = atomicsignal.SignalReceivedError

// Notifier aliases [atomicsignal.Notifier] so references stay stable as
// `cmdsignals.Notifier`. Its methods — Run, Stop, Swap, Restore — are
// documented upstream.
type Notifier = atomicsignal.Notifier

// Handler aliases [atomicsignal.Handler] so references stay stable as
// `cmdsignals.Handler`; build one with [NewHandler].
type Handler = atomicsignal.Handler

// NotifyContext is [atomicsignal.NotifyContext] with [ExitSignals] baked in
// as the canceling set: ctx is cancelled with [*SignalReceivedError] when one
// of [ExitSignals] is received.
//
// [atomicsignal.Notifier.Run] must be called, in another goroutine, to make
// signal propagation work. Callers should defer [atomicsignal.Notifier.Stop]
// and cancel to release resources (Stop also makes Run return); use explicit
// calls instead when a code path exits via [os.Exit], which skips defers:
//
//	n, ctx, cancel := cmdsignals.NotifyContext(ctx)
//	defer cancel(nil)
//	go n.Run()
//	defer n.Stop()
//
// Callers are advised to check errors with [context.Cause].
//
//	err := work(ctx)
//	if err != nil {
//		if errors.Is(err, ctx.Err()) {
//			if sigErr, ok := errors.AsType[*SignalReceivedError](context.Cause(ctx));  ok {
//				// log as signal cancellation using sigErr.Sig
//				// or print nothing and exit as if normal exit
//				return
//			}
//		}
//		// non-cancellation error.
//	}
//
// The registered signal set is fixed for the Notifier's lifetime; pass
// signals to additionally register non-canceling ones up front when a
// swapped-in handler ([atomicsignal.Notifier.Swap]) may need them later.
//
// The Notifier is stored in ctx; retrieve it with [CtxValueNotifier].
func NotifyContext(
	inCtx context.Context,
	signals ...os.Signal,
) (n *Notifier, ctx context.Context, cancel context.CancelCauseFunc) {
	return atomicsignal.NotifyContext(inCtx, ExitSignals[:], signals...)
}

// CtxValueNotifier proxies [atomicsignal.CtxValueNotifier], returning the
// [Notifier] stored in ctx by [NotifyContext], so references stay stable as
// `cmdsignals.CtxValueNotifier`.
func CtxValueNotifier(ctx context.Context) (*Notifier, bool) {
	return atomicsignal.CtxValueNotifier(ctx)
}

// NewHandler proxies [atomicsignal.NewHandler] so references stay stable as
// `cmdsignals.NewHandler`; use it to build the [Handler] swapped in with
// [atomicsignal.Notifier.Swap]. The caller is responsible for running it
// (`go h.Run()`) and stopping it.
func NewHandler(
	buffer int,
	defaultHandler func(sig os.Signal),
	handlers map[os.Signal]func(sig os.Signal),
) *Handler {
	return atomicsignal.NewHandler(buffer, defaultHandler, handlers)
}
