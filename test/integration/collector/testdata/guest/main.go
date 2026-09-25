// Command guest stands in for a long-running workload: a hypervisor process
// or a containerised service. The collector suite copies it under the name it
// needs -- qemu-system-x86_64, firecracker, nginx -- and runs it with the
// command line it needs, which it ignores.
//
// With GUEST_VCPUS=n it names n of its threads "CPU i/KVM", the way qemu names
// its vCPU threads. It prints "ready" once they are named, and exits 0 on
// SIGTERM or SIGINT.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func main() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	vcpus, err := strconv.Atoi(os.Getenv("GUEST_VCPUS"))
	if err != nil {
		vcpus = 0
	}
	named := make(chan error)
	for i := range vcpus {
		go func() {
			// A thread of its own for as long as the process runs, so the name
			// stays on a thread nothing else is scheduled onto.
			runtime.LockOSThread()
			comm := "/proc/self/task/" + strconv.Itoa(unix.Gettid()) + "/comm"
			named <- os.WriteFile(comm, []byte("CPU "+strconv.Itoa(i)+"/KVM"), 0)
			select {}
		}()
	}
	for range vcpus {
		if err := <-named; err != nil {
			fmt.Fprintln(os.Stderr, "guest: name a vCPU thread:", err)
			os.Exit(1)
		}
	}
	fmt.Println("ready")
	<-stop
}
