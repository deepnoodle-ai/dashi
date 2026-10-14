package realtime_test

import (
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/realtime"
)

func TestErrorMessage(t *testing.T) {
	cause := errors.New("socket reset")
	for _, c := range []struct {
		err  *realtime.Error
		want string
	}{
		{&realtime.Error{Category: realtime.ErrorProtocol}, "realtime: protocol"},
		{&realtime.Error{Category: realtime.ErrorProvider, Code: "rate_limit"}, "realtime: provider (rate_limit)"},
		{&realtime.Error{Category: realtime.ErrorTransport, Cause: cause}, "realtime: transport: socket reset"},
		{&realtime.Error{Category: realtime.ErrorProvider, Code: "x", Cause: cause}, "realtime: provider (x): socket reset"},
	} {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}
