// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

/*
#include <utmp.h>
*/
import "C"

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/locksmith/Godeps/_workspace/src/github.com/coreos/go-systemd/dbus"
	"github.com/coreos/locksmith/Godeps/_workspace/src/github.com/coreos/go-systemd/login1"

	"github.com/coreos/locksmith/lock"
	"github.com/coreos/locksmith/pkg/machineid"
	"github.com/coreos/locksmith/pkg/timeutil"
	"github.com/coreos/locksmith/updateengine"
)

const (
	initialInterval   = time.Second * 5
	maxInterval       = time.Minute * 5
	loginsRebootDelay = time.Minute * 5
)

var (
	etcdServices = []string{
		"etcd.service",
		"etcd2.service",
	}
)

const (
	StrategyReboot     = "reboot"
	StrategyEtcdLock   = "etcd-lock"
	StrategyBestEffort = "best-effort"
	StrategyOff        = "off"
)

// attempt to broadcast msg to all lines registered in utmp
// returns count of lines successfully opened (and likely broadcasted to)
func broadcast(msg string) uint {
	var cnt uint
	C.setutent()

	for {
		var utmp *C.struct_utmp
		utmp = C.getutent()
		if utmp == nil {
			break
		}

		line := C.GoString(&utmp.ut_line[0])
		tty, _ := os.OpenFile("/dev/"+line, os.O_WRONLY, 0)
		if tty == nil {
			// ignore errors, this is just a best-effort courtesy notice
			continue
		}
		cnt++
		go func() {
			fmt.Fprintf(tty, "\r%79s\r\n", " ")
			fmt.Fprintf(tty, "%-79.79s\007\007\r\n", fmt.Sprintf("Broadcast message from locksmithd at %s:", time.Now()))
			fmt.Fprintf(tty, "%-79.79s\r\n", msg) // msg is assumed to be short and not require wrapping
			fmt.Fprintf(tty, "\r%79s\r\n", " ")
			tty.Close()
		}()
	}

	return cnt
}

func expBackoff(interval time.Duration) time.Duration {
	interval = interval * 2
	if interval > maxInterval {
		interval = maxInterval
	}
	return interval
}

func rebootAndSleep(lgn *login1.Conn) {
	// Broadcast a notice, if broadcast found lines to notify, delay the reboot.
	delaymins := loginsRebootDelay / time.Minute
	lines := broadcast(fmt.Sprintf("System reboot in %d minutes!", delaymins))
	if 0 != lines {
		fmt.Printf("Logins detected, delaying reboot for %d minutes.\n", delaymins)
		time.Sleep(loginsRebootDelay)
	}
	lgn.Reboot(false)
	fmt.Println("Reboot sent. Going to sleep.")

	// Wait a really long time for the reboot to occur.
	time.Sleep(time.Hour * 24 * 7)
}

// lockAndReboot attempts to acquire the lock and reboot the machine in an
// infinite loop. Returns if the reboot failed.
func (r rebooter) lockAndReboot(lck *lock.Lock) {
	interval := initialInterval
	for {
		err := lck.Lock()
		if err != nil && err != lock.ErrExist {
			interval = expBackoff(interval)
			fmt.Printf("Retrying in %v. Error locking: %v\n", interval, err)
			time.Sleep(interval)

			continue
		}

		rebootAndSleep(r.lgn)

		return
	}
}

func setupLock() (lck *lock.Lock, err error) {
	elc, err := getClient()
	if err != nil {
		return nil, fmt.Errorf("Error initializing etcd client: %v", err)
	}

	mID := machineid.MachineID("/")
	if mID == "" {
		return nil, fmt.Errorf("Cannot read machine-id")
	}

	lck = lock.New(mID, elc)

	return lck, nil
}

// etcdActive returns true if etcd is not in an inactive state according to systemd.
func etcdActive() (active bool, name string, err error) {
	active = false
	name = ""

	sys, err := dbus.New()
	if err != nil {
		return
	}
	defer sys.Close()

	for _, service := range etcdServices {
		prop, err := sys.GetUnitProperty(service, "ActiveState")
		if err != nil {
			continue
		}

		switch prop.Value.Value().(string) {
		case "inactive":
			continue
		default:
			active = true
			name = service
			break
		}
	}

	return
}

type rebooter struct {
	strategy string
	lgn      *login1.Conn
}

func (r rebooter) useLock() (useLock bool, err error) {
	switch r.strategy {
	case StrategyBestEffort:
		active, name, err := etcdActive()
		if err != nil {
			return false, err
		}
		if active {
			fmt.Printf("%s is active\n", name)
			useLock = true
		} else {
			fmt.Printf("%v are inactive\n", etcdServices)
			useLock = false
		}
	case StrategyEtcdLock:
		useLock = true
	case StrategyReboot:
		useLock = false
	default:
		return false, fmt.Errorf("Unknown strategy: %s", r.strategy)
	}

	return useLock, nil
}

func (r rebooter) reboot() int {
	useLock, err := r.useLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if useLock {
		lck, err := setupLock()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		err = unlockIfHeld(lck)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}

		r.lockAndReboot(lck)
	}

	rebootAndSleep(r.lgn)
	fmt.Println("Error: reboot attempt never finished")
	return 1
}

// unlockIfHeld will unlock a lock, if it is held by this machine, or return an error.
func unlockIfHeld(lck *lock.Lock) error {
	err := lck.Unlock()
	if err == lock.ErrNotExist {
		return nil
	} else if err == nil {
		fmt.Println("Unlocked existing lock for this machine")
		return nil
	}

	return err
}

// unlockHeldLock will loop until it can confirm that any held locks are
// released or a stop signal is sent.
func unlockHeldLocks(stop chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	interval := initialInterval
	for {
		var reason string
		select {
		case <-stop:
			return
		case <-time.After(interval):
			active, _, err := etcdActive()
			if err != nil {
				reason = fmt.Sprintf("error checking status of %v", etcdServices)
				break
			}
			if !active {
				_, err := getClient()
				if err != nil {
					reason = fmt.Sprintf("%v are inactive and remote cluster not available", etcdServices)
					break
				}
			}

			lck, err := setupLock()
			if err != nil {
				reason = "error setting up lock: " + err.Error()
				break
			}

			err = unlockIfHeld(lck)
			if err == nil {
				return
			}
			reason = err.Error()
		}

		interval = expBackoff(interval)
		fmt.Printf("Unlocking old locks failed: %v. Retrying in %v.\n", reason, interval)
	}
}

// runDaemon waits for the reboot needed signal coming out of update engine and
// attempts to acquire the reboot lock. If the reboot lock is acquired then the
// machine will reboot.
func runDaemon() int {
	var period *timeutil.Periodic

	strategy := os.Getenv("REBOOT_STRATEGY")

	if strategy == "" {
		strategy = StrategyBestEffort
	}

	if strategy == StrategyOff {
		fmt.Fprintf(os.Stderr, "Reboot strategy is %q - shutting down.\n", strategy)
		return 0
	}

	startw := os.Getenv("REBOOT_WINDOW_START")
	lengthw := os.Getenv("REBOOT_WINDOW_LENGTH")
	if (startw == "") != (lengthw == "") {
		fmt.Fprintln(os.Stderr, "either both or neither $REBOOT_WINDOW_START and $REBOOT_WINDOW_LENGTH must be set")
		return 1
	}

	if startw != "" && lengthw != "" {
		p, err := timeutil.ParsePeriodic(startw, lengthw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error parsing reboot window: %s\n", err)
			return 1
		}

		period = p
	}

	shutdown := make(chan os.Signal, 1)
	stop := make(chan struct{}, 1)

	go func() {
		<-shutdown
		fmt.Fprintln(os.Stderr, "Received interrupt/termination signal - shutting down.")
		os.Exit(0)
	}()
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	ue, err := updateengine.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error initializing update1 client:", err)
		return 1
	}

	lgn, err := login1.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error initializing login1 client:", err)
		return 1
	}

	var wg sync.WaitGroup
	if strategy != StrategyReboot {
		wg.Add(1)
		go unlockHeldLocks(stop, &wg)
	}

	ch := make(chan updateengine.Status, 1)
	go ue.RebootNeededSignal(ch, stop)

	r := rebooter{strategy, lgn}

	result, err := ue.GetStatus()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Cannot get update engine status:", err)
		return 1
	}

	fmt.Printf("locksmithd starting currentOperation=%q strategy=%q\n",
		result.CurrentOperation,
		strategy,
	)

	if result.CurrentOperation != updateengine.UpdateStatusUpdatedNeedReboot {
		<-ch
	}

	close(stop)
	wg.Wait()

	if period != nil {
		now := time.Now()
		sleeptime := period.DurationToStart(now)
		if sleeptime > 0 {
			fmt.Printf("Waiting for %s to reboot.\n", sleeptime)
			time.Sleep(sleeptime)
		}
	}

	return r.reboot()
}
