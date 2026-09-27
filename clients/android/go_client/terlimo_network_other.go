//go:build !android || !cgo

package main

import "errors"

func bindManagedNetwork(handle uint64) error { return errors.New("ANDROID_NETWORK_REQUIRED") }
