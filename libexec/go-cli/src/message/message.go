package message

import (
	"fmt"
)

type messageLevel int

const (
	ABRT messageLevel = -4
	ERROR messageLevel = -3
	WARNING messageLevel = -2
	LOG messageLevel = -1
	INFO messageLevel = 1
	VERBOSE messageLevel = 2
	VERBOSE2 messageLevel = 3
	VERBOSE3 messageLevel = 4
	DEBUG messageLevel = 5
)

var logLevel messageLevel

func MessageInit(level int) {
	
}

func Message(level messageLevel, format string,) {

}
