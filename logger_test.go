package ads

import (
	"log/slog"
	"testing"

	"github.com/siyka-au/go-ads/v3/internal/testlog"
)

// TestTestLogHandler_QualifiesGroups covers the capture helper itself: a handler
// that silently flattened groups could make an assertion pass on a key the real
// handler would have written as "group.key".
func TestTestLogHandler_QualifiesGroups(t *testing.T) {
	logs := &testlog.Handler{}
	lg := slog.New(logs)

	lg.WithGroup("net").With("port", 48898).Info("dialed", "peer", "10.0.0.1")
	lg.Info("inline", slog.Group("drop", slog.Int("frames", 12)))
	lg.Info("emptyGroupKeyInlines", slog.Group("", slog.String("k", "v")))
	lg.Info("emptyGroupIgnored", slog.Group("g"))

	rec := logs.FindByMessage("dialed")
	if rec == nil {
		t.Fatal("no record captured")
	}
	if got := rec.Attr("net.port"); got != "48898" {
		t.Errorf("WithGroup+With key = %q, want net.port=48898 (attrs: %v)", got, rec.Attrs)
	}
	if got := rec.Attr("net.peer"); got != "10.0.0.1" {
		t.Errorf("record attr under group = %q, want net.peer=10.0.0.1 (attrs: %v)", got, rec.Attrs)
	}

	inline := logs.FindByMessage("inline")
	if got := inline.Attr("drop.frames"); got != "12" {
		t.Errorf("slog.Group key = %q, want drop.frames=12 (attrs: %v)", got, inline.Attrs)
	}

	if got := logs.FindByMessage("emptyGroupKeyInlines").Attr("k"); got != "v" {
		t.Errorf("an empty group key must inline its attributes, got %q", got)
	}
	if r := logs.FindByMessage("emptyGroupIgnored"); len(r.Attrs) != 0 {
		t.Errorf("a group with no attributes must be ignored, got %v", r.Attrs)
	}
}

// TestTestLogHandler_GroupQualifiesOnlyLaterAttrs covers the reverse call order
// of TestTestLogHandler_QualifiesGroups: a group opened after an attribute must
// not reach back and qualify it. A handler that applied the group prefix that
// happened to be open at Handle time would record "net.request_id" here, and a
// test asserting on "request_id" would then fail for a reason that exists only
// in the helper.
func TestTestLogHandler_GroupQualifiesOnlyLaterAttrs(t *testing.T) {
	logs := &testlog.Handler{}
	lg := slog.New(logs)

	lg.With("request_id", "abc").WithGroup("net").With("port", 48898).Info("dialed")

	rec := logs.FindByMessage("dialed")
	if rec == nil {
		t.Fatal("no record captured")
	}
	if got, want := rec.Attr("request_id"), "abc"; got != want {
		t.Errorf("attr added before the group = %q, want request_id=%q (attrs: %v)", got, want, rec.Attrs)
	}
	if rec.HasAttr("net.request_id") {
		t.Errorf("a group must not qualify an attribute added before it (attrs: %v)", rec.Attrs)
	}
	if got, want := rec.Attr("net.port"), "48898"; got != want {
		t.Errorf("attr added after the group = %q, want net.port=%q (attrs: %v)", got, want, rec.Attrs)
	}
}

// groupLogValuer is a slog.LogValuer whose LogValue returns a group, the case
// that separates resolving from not resolving.
type groupLogValuer struct{ port int }

func (g groupLogValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("port", g.port), slog.String("proto", "tcp"))
}

// TestTestLogHandler_ResolvesLogValuer: the capture helper must resolve values
// before inspecting their kind. secret in this package is a slog.LogValuer, so
// an unresolved value would be stored as one opaque scalar — and a redaction
// assertion could pass against text the real handler never wrote.
func TestTestLogHandler_ResolvesLogValuer(t *testing.T) {
	logs := &testlog.Handler{}
	lg := slog.New(logs)

	lg.Info("valued", "conn", groupLogValuer{port: 48898})
	lg.Info("secret", "password", secret("hunter2"))

	rec := logs.FindByMessage("valued")
	if rec == nil {
		t.Fatal("no record captured")
	}
	if got, want := rec.Attr("conn.port"), "48898"; got != want {
		t.Errorf("LogValuer group child = %q, want conn.port=%q (attrs: %v)", got, want, rec.Attrs)
	}
	if got, want := rec.Attr("conn.proto"), "tcp"; got != want {
		t.Errorf("LogValuer group child = %q, want conn.proto=%q (attrs: %v)", got, want, rec.Attrs)
	}
	if rec.HasAttr("conn") {
		t.Errorf("an unresolved LogValuer was stored as a scalar (attrs: %v)", rec.Attrs)
	}

	pw := logs.FindByMessage("secret")
	if got, want := pw.Attr("password"), "[REDACTED]"; got != want {
		t.Errorf("secret attr = %q, want %q (attrs: %v)", got, want, pw.Attrs)
	}
}
