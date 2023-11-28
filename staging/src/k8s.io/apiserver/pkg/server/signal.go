/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"context"
	"os"
	"os/signal"
)

// <gotrick>: If we queue up lots of goroutines, we can have them all start at the same time
// by closing the signal channel.
// the SIGHUP signal is sent when a program loses its controlling terminal.
// The SIGINT signal is sent when the user at the controlling terminal presses the interrupt character
var onlyOneSignalHandler = make(chan struct{})
// channel for sending OS signals (SIGINT, SIGTERM), to be initialized later. Recommended to be buffered
var shutdownHandler chan os.Signal   


// SetupSignalHandler registered for SIGTERM and SIGINT. A stop channel is returned
// which is closed on one of these signals. If a second signal is caught, the program
// is terminated with exit code 1.
// Only one of SetupSignalContext and SetupSignalHandler should be called, and only can
// be called once.
// <gotrick>Usage of context package to signal the completion of a task executed in a goroutine by calling 
// cancel() function created by the context.WithCancel(...) sends a value to the channel ctx.Done() 
// that will xyz (ex: end the for loop and exit.)

func SetupSignalHandler() <-chan struct{} {
	return SetupSignalContext().Done()
}

// SetupSignalContext is same as SetupSignalHandler, but a context.Context is returned.
// Only one of SetupSignalContext and SetupSignalHandler should be called, and only can
// be called once.
func SetupSignalContext() context.Context {
	close(onlyOneSignalHandler) // panics when called twice

	shutdownHandler = make(chan os.Signal, 2)

	ctx, cancel := context.WithCancel(context.Background())
	// Notify disables the default behavior for a given set of asynchronous signals and instead 
	// delivers them over one or more registered channels. Specifically, it applies to the signals 
	// SIGHUP, SIGINT, SIGQUIT, SIGABRT, and SIGTERM
	signal.Notify(shutdownHandler, shutdownSignals...)  // registering channel to receive 
	go func() {
		<-shutdownHandler
		cancel()
		<-shutdownHandler
		os.Exit(1) // second signal. Exit directly.
	}()

	return ctx
}

// RequestShutdown emulates a received event that is considered as shutdown signal (SIGTERM/SIGINT)
// This returns whether a handler was notified
func RequestShutdown() bool {
	if shutdownHandler != nil {
		select {
		case shutdownHandler <- shutdownSignals[0]:
			return true
		default:
		}
	}

	return false
}
