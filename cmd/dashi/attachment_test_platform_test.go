package main

import "runtime"

func attachmentPlatformUnsupported() bool { return runtime.GOOS != "darwin" && runtime.GOOS != "linux" }
