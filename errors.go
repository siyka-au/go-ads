package ads

import (
	"errors"
)

// ErrDisconnected indicates the underlying TCP connection is not available —
// either Close() has been called or a reconnect has failed. Callers should use
// errors.Is(err, ErrDisconnected) to detect this case.
var ErrDisconnected = errors.New("connection is disconnected")

// ErrRuntimeNotRunning is returned by symbol and subscription calls when the
// system service reports the runtime is not in RUN. A refusal, not a retry: in
// CONFIG the runtime port does not exist, so the call cannot succeed and attempting
// it only yields a misleading AMS "port not found". The session keeps polling.
var ErrRuntimeNotRunning = errors.New("ads: PLC runtime is not in RUN")
