//go:build windows

// DeskFerry's Windows executable is intentionally multi-mode.  The normal
// process owns the control-panel/tray UI while Windows Service Manager starts
// the same executable for the privileged Work and Home-network components.
package main

import (
	"os"
	"strings"

	homewindows "deskferry/windows/home"
	homenetworkservice "deskferry/windows/networkservice"
	windowssetup "deskferry/windows/setup"
	workservice "deskferry/windows/workservice"
	workconfigurator "deskferry/windows/workui"
)

const (
	modeWorkConfigurator = "-work-configurator"
	modeWindowsSetup     = "-windows-setup"
	modeHomeNetwork      = "-home-network-service"
	// modeWorkConsole runs the Work agent in the foreground. Plain -console
	// belongs to the Home app, so the Work agent's Scheduled Task fallback
	// (used when the Windows service cannot be created) launches this mode.
	modeWorkConsole = "-work-console"
)

func main() {
	switch {
	case consumeMode(modeWorkConfigurator):
		workconfigurator.Main()
	case consumeMode(modeWindowsSetup):
		windowssetup.Main()
	case consumeMode(modeHomeNetwork):
		homenetworkservice.Main()
	case consumeMode(modeWorkConsole):
		os.Args = append(os.Args, "-console")
		workservice.Main()
	case workServiceInvocation(os.Args[1:]):
		workservice.Main()
	default:
		homewindows.Main()
	}
}

func consumeMode(mode string) bool {
	for index, arg := range os.Args[1:] {
		if !strings.EqualFold(arg, mode) {
			continue
		}
		actual := index + 1
		os.Args = append(os.Args[:actual], os.Args[actual+1:]...)
		return true
	}
	return false
}

func workServiceInvocation(args []string) bool {
	for _, arg := range args {
		name := strings.ToLower(strings.SplitN(arg, "=", 2)[0])
		switch name {
		case "-service", "-install", "-uninstall", "-status", "-self-test", "-update-service", "-screen-capture-helper":
			return true
		}
	}
	return false
}
