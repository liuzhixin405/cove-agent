package race

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRaceProcessTreeHelper(t *testing.T) {
	mode := os.Args[len(os.Args)-1]
	if mode == "race-grandchild" {
		fmt.Println(os.Getpid())
		<-time.NewTimer(24 * time.Hour).C
		return
	}
	if mode != "race-tree-exit" && mode != "race-tree-wait" {
		return
	}
	executable, _ := os.Executable()
	child := exec.Command(executable, "-test.run=^TestRaceProcessTreeHelper$", "--", "race-grandchild")
	stdout, err := child.StdoutPipe()
	if err != nil || child.Start() != nil {
		os.Exit(20)
	}
	pid, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		os.Exit(21)
	}
	if os.WriteFile("child.pid", []byte(strings.TrimSpace(pid)), 0600) != nil {
		os.Exit(22)
	}
	if mode == "race-tree-wait" {
		<-time.NewTimer(24 * time.Hour).C
	}
	os.Exit(0)
}

func TestWindowsProcessTreeReaped(t *testing.T) {
	for _, mode := range []string{"race-tree-exit", "race-tree-wait"} {
		directory := t.TempDir()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		result := Execute(ctx, directory, nil, []string{executable, "-test.run=^TestRaceProcessTreeHelper$", "--", mode})
		cancel()
		want := "passed"
		if mode == "race-tree-wait" {
			want = "timeout"
		}
		if result.Status != want {
			t.Fatalf("%s: %+v", mode, result)
		}
		data, err := os.ReadFile(filepath.Join(directory, "child.pid"))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err == windows.ERROR_INVALID_PARAMETER {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		status, err := windows.WaitForSingleObject(handle, 2000)
		windows.CloseHandle(handle)
		if err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatalf("descendant %d still alive: %d %v", pid, status, err)
		}
	}
}
