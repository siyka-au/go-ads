// Package ads talks to Beckhoff TwinCAT PLCs over ADS.
//
// A Session is a connection to one PLC runtime that survives network drops and
// PLC restarts: it resolves symbols by name, reads and writes them as Go
// values, and keeps notification subscriptions alive across reconnects.
//
//	sess, err := ads.NewSession(ctx, ads.Endpoint{Host: "192.168.1.10"})
//	...
//	err = sess.Connect(ctx)
//	v, err := sess.ReadValue(ctx, "MAIN.counter")
//
// The other packages cover what a Session builds on:
//
//   - ams: the protocol's vocabulary (addresses, ports, index groups, return
//     codes, states, data types).
//   - adsclient: a raw ADS client, one TCP connection and no cache or
//     reconnect, for index-group level access and tooling.
//   - router: the AMS router's UDP services, Identify and AddRoute.
package ads
