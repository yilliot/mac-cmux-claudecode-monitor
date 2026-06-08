module macmonitor

go 1.26

require (
	github.com/caseymrm/menuet v1.0.2
	github.com/shirou/gopsutil/v3 v3.24.5
)

// Local copy patched to support compact two-line menu bar titles.
replace github.com/caseymrm/menuet => ./third_party/menuet

require (
	github.com/caseymrm/askm v1.0.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/power-devops/perfstat v0.0.0-20210106213030-5aafc221ea8c // indirect
	github.com/shoenig/go-m1cpu v0.1.6 // indirect
	github.com/tklauser/go-sysconf v0.3.12 // indirect
	github.com/tklauser/numcpus v0.6.1 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/sys v0.20.0 // indirect
)
