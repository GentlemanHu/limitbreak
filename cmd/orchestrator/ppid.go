package main

import "quota-session-orchestrator/internal/host"

// parentPID reads the parent from the kernel process table; -1 on failure.
var parentPID = host.ParentPID

// executablePath reads the file a process executes; "" on failure.
var executablePath = host.ExecutablePath
