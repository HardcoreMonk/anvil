package storage

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTeardownDMSnapshotContinuesAfterDMRemoveFailure(t *testing.T) {
	for _, keepStore := range []bool{false, true} {
		name := "remove-store"
		if keepStore {
			name = "keep-store"
		}
		t.Run(name, func(t *testing.T) {
			var commands []string
			var removedPaths []string
			nowCalls := 0
			start := time.Unix(0, 0)
			ops := dmSnapshotTeardownOps{
				run: func(command string, args ...string) ([]byte, error) {
					call := command + " " + strings.Join(args, " ")
					commands = append(commands, call)
					switch command {
					case "dmsetup":
						return []byte("device busy"), errors.New("forced dm failure")
					case "losetup":
						return []byte("still referenced"), errors.New("forced loop failure")
					default:
						return nil, nil
					}
				},
				remove: func(path string) error {
					removedPaths = append(removedPaths, path)
					return errors.New("forced store failure")
				},
				now: func() time.Time {
					nowCalls++
					if nowCalls == 1 {
						return start
					}
					return start.Add(11 * time.Second)
				},
				sleep: func(time.Duration) {},
			}
			info := &DMSnapshotInfo{
				LoopDevice:     "/dev/loop-base",
				COWLoopDevice:  "/dev/loop-cow",
				DMDevice:       "cow-vm-test.cow",
				ExceptionStore: "/workspace/vm-test.cow",
				MountTarget:    "/workspace/vm-test.ext4",
			}

			err := teardownDMSnapshotWithOps(info, keepStore, ops)
			if err == nil {
				t.Fatal("teardown error = nil, want forced failures")
			}
			joined := strings.Join(commands, "\n")
			for _, want := range []string{
				"dmsetup remove --retry cow-vm-test.cow",
				"losetup -d /dev/loop-cow",
				"losetup -d /dev/loop-base",
			} {
				if !strings.Contains(joined, want) {
					t.Fatalf("commands missing %q after forced dm failure:\n%s", want, joined)
				}
			}
			for _, want := range []string{"dmsetup remove", "/dev/loop-cow", "/dev/loop-base"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing %q", err, want)
				}
			}
			if keepStore {
				if len(removedPaths) != 0 {
					t.Fatalf("keep-store teardown removed paths: %v", removedPaths)
				}
				if strings.Contains(err.Error(), "forced store failure") {
					t.Fatalf("keep-store error unexpectedly contains store removal: %v", err)
				}
			} else {
				if len(removedPaths) != 1 || removedPaths[0] != info.ExceptionStore {
					t.Fatalf("removed paths = %v, want [%s]", removedPaths, info.ExceptionStore)
				}
				if !strings.Contains(err.Error(), "forced store failure") {
					t.Fatalf("error %q missing store removal failure", err)
				}
			}
		})
	}
}
