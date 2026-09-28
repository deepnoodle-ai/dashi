package kerneltest_test

import (
	"testing"

	"github.com/deepnoodle-ai/kernel"
	"github.com/deepnoodle-ai/kernel/kerneltest"
)

func TestMemoryRecorderConforms(t *testing.T) {
	kerneltest.Run(t, func(*testing.T) kerneltest.Store {
		return kernel.NewMemoryRecorder()
	})
}
