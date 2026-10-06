package defaultclient_test

import (
	"errors"
	"fmt"

	"github.com/deepnoodle-ai/dashi/decide/defaultclient"
)

// A host checks the environment at startup, before the first check runs,
// so it can warn once or refuse to start.
func ExampleLoad() {
	setup, err := defaultclient.Load()
	switch {
	case errors.Is(err, defaultclient.ErrSetup):
		fmt.Println("decide checks are off:", err)
	case err == nil:
		fmt.Println("decide checks use", setup.Provider)
	}
}
