package main

import "strings"

// How the service identifies itself to the platform's service manager.
//
// The name is what `sc.exe`, `Get-Service` and `systemctl` take; the display
// name and the description are what an operator reads in services.msc, and
// they are the only explanation of this process anyone will go looking for.
const (
	serviceName        = "NanoASR"
	serviceDisplayName = "NanoASR speech recognition server"
	serviceDescription = "Offline speech recognition. Serves the OpenAI, native and ERA HTTP dialects on one port."
)

// serviceVerbs are the subcommands of `nanoasr service`, in the order the help
// lists them.
var serviceVerbs = []string{"install", "uninstall", "start", "stop", "restart", "status"}

// serviceFlag maps the flag spelling of a service verb onto the subcommand, so
// that `nanoasr --install` and `nanoasr service install` are one command.
//
// Both spellings exist because both are expected: a Windows program that
// registers itself is invoked as `program.exe --install`, and every other
// command this binary has is a bare verb. Accepting one and not the other
// would make somebody read the help to find out which.
// listVerbs renders the accepted subcommands for an error message.
func listVerbs() string {
	return strings.Join(serviceVerbs[:len(serviceVerbs)-1], ", ") + " or " + serviceVerbs[len(serviceVerbs)-1]
}

func serviceFlag(arg string) (string, bool) {
	verb := strings.TrimLeft(arg, "-")
	if verb == arg {
		return "", false // not a flag at all
	}
	for _, v := range serviceVerbs {
		if verb == v {
			return v, true
		}
	}
	// "remove" is not offered in the help, but sc.exe spells it that way and
	// an operator who types it means uninstall.
	if verb == "remove" {
		return "uninstall", true
	}
	return "", false
}
