// +build linux

/*
Copyright 2019 Gravitational, Inc.

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

package bpf

import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"unsafe"

	"github.com/gravitational/trace"

	"github.com/iovisor/gobpf/bcc"
)

// rawExecEvent is sent by the eBPF program that Teleport pulls off the perf
// buffer.
type rawExecEvent struct {
	// PID is the ID of the process.
	PID uint64

	// PPID is the PID of the parent process.
	PPID uint64

	// Command is the executable.
	Command [commMax]byte

	// Type is the type of event.
	Type int32

	// Argv is the list of arguments to the program.
	Argv [argvMax]byte

	// ReturnCode is the return code of execve.
	ReturnCode int32

	// CgroupID is the internal cgroupv2 ID of the event.
	CgroupID uint64
}

//// execEvent is the parsed execve event.
//type execEvent struct {
//	// PID is the ID of the process.
//	PID uint64
//
//	// PPID is the PID of the parent process.
//	PPID uint64
//
//	// Program is name of the executable.
//	Program string
//
//	// Path is the full path to the executable.
//	Path string
//
//	// Argv is the list of arguments to the program. Note, the first element does
//	// not contain the name of the process.
//	Argv []string
//
//	// ReturnCode is the return code of execve.
//	ReturnCode int32
//
//	// CgroupID is the internal cgroupv2 ID of the event.
//	CgroupID uint64
//}

type exec struct {
	closeContext context.Context

	eventsCh chan []byte

	perfMaps []*bcc.PerfMap
	module   *bcc.Module
}

func newExec(closeContext context.Context) (*exec, error) {
	e := &exec{
		closeContext: closeContext,
	}

	err := e.start()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return e, nil
}

func (e *exec) start() error {
	var err error

	e.module = bcc.NewModule(execveSource, []string{})
	if e.module == nil {
		return trace.BadParameter("failed to load libbcc")
	}

	// Hook execve syscall.
	err = attachProbe(e.module, bcc.GetSyscallFnName("execve"), "syscall__execve")
	if err != nil {
		return trace.Wrap(err)
	}
	err = attachRetProbe(e.module, bcc.GetSyscallFnName("execve"), "do_ret_sys_execve")
	if err != nil {
		return trace.Wrap(err)
	}

	// Open perf buffer and start processing execve events.
	e.eventCh, err = openPerfBuffer(e.module, e.perfMaps, "execve_events")
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func (e *exec) close() {
	for _, perfMap := range e.perfMaps {
		perfMap.Stop()
	}
	e.module.Close()
}

func (e *exec) eventsCh() <-chan []byte {
	return e.eventsCh
}

const execveSource string = `
#include <uapi/linux/ptrace.h>
#include <linux/sched.h>
#include <linux/fs.h>

#define ARGSIZE  128

enum event_type {
    EVENT_ARG,
    EVENT_RET,
};

struct data_t {
    // pid as in the userspace term (i.e. task->tgid in kernel).
    u64 pid;
    // ppid is the userspace term (i.e task->real_parent->tgid in kernel).
    u64 ppid;
    char comm[TASK_COMM_LEN];
    enum event_type type;
    char argv[ARGSIZE];
    int retval;
    u64 cgroup;
};

BPF_PERF_OUTPUT(execve_events);

static int __submit_arg(struct pt_regs *ctx, void *ptr, struct data_t *data)
{
    bpf_probe_read(data->argv, sizeof(data->argv), ptr);
    execve_events.perf_submit(ctx, data, sizeof(struct data_t));
    return 1;
}

static int submit_arg(struct pt_regs *ctx, void *ptr, struct data_t *data)
{
    const char *argp = NULL;
    bpf_probe_read(&argp, sizeof(argp), ptr);
    if (argp) {
        return __submit_arg(ctx, (void *)(argp), data);
    }
    return 0;
}

int syscall__execve(struct pt_regs *ctx,
    const char __user *filename,
    const char __user *const __user *__argv,
    const char __user *const __user *__envp)
{
    // create data here and pass to submit_arg to save stack space (#555)
    struct data_t data = {};
    struct task_struct *task;

    data.pid = bpf_get_current_pid_tgid() >> 32;

    task = (struct task_struct *)bpf_get_current_task();
    // Some kernels, like Ubuntu 4.13.0-generic, return 0
    // as the real_parent->tgid.
    // We use the getPpid function as a fallback in those cases.
    // See https://github.com/iovisor/bcc/issues/1883.
    data.ppid = task->real_parent->tgid;

    bpf_get_current_comm(&data.comm, sizeof(data.comm));
    data.type = EVENT_ARG;

    __submit_arg(ctx, (void *)filename, &data);

    // skip first arg, as we submitted filename
    #pragma unroll
    for (int i = 1; i < 20; i++) {
        if (submit_arg(ctx, (void *)&__argv[i], &data) == 0)
             goto out;
    }

    // handle truncated argument list
    char ellipsis[] = "...";
    __submit_arg(ctx, (void *)ellipsis, &data);
out:
    return 0;
}

int do_ret_sys_execve(struct pt_regs *ctx)
{
    struct data_t data = {};
    struct task_struct *task;

    data.pid = bpf_get_current_pid_tgid() >> 32;
    data.cgroup = bpf_get_current_cgroup_id();

    task = (struct task_struct *)bpf_get_current_task();
    // Some kernels, like Ubuntu 4.13.0-generic, return 0
    // as the real_parent->tgid.
    // We use the getPpid function as a fallback in those cases.
    // See https://github.com/iovisor/bcc/issues/1883.
    data.ppid = task->real_parent->tgid;

    bpf_get_current_comm(&data.comm, sizeof(data.comm));
    data.type = EVENT_RET;
    data.retval = PT_REGS_RC(ctx);
    execve_events.perf_submit(ctx, &data, sizeof(data));

    return 0;
}`
