package main

import (
    "fmt"
    "os"
    "strconv"
    "strings"
)

func main() {
    args := os.Args[1:]

    outer:
        for i, arg := range args {
            switch arg {
                case "-h", "--help":
                    // exec $SINGULARITY_libexecdir/singularity/cli/help.exec "$@"
                    fmt.Printf("Would print help for %s\n", args[i+1])
                case "-q", "--quiet":
                    os.Setenv("SINGULARITY_MESSAGELEVEL", "0")
                case "--version":
                    fmt.Printf("%s\n", os.Getenv("SINGULARITY_version"))
                    os.Exit(0)
                case "-s", "--silent":
                    os.Setenv("SINGULARITY_MESSAGELEVEL", "-3")
                case "-d", "--debug":
                    os.Setenv("SINGULARITY_MESSAGELEVEL", "5")
                    fmt.Printf("Enabling debugging\n")
                case "-x", "--sh-debug":
                    os.Setenv("SHELL_DEBUG", "1")
                    fmt.Printf("Enabling shell debugging\n")
                case "-v", "--verbose":
                    increaseverbosity(1)
                case "-vv":
                    increaseverbosity(2)
                case "-vvv":
                    increaseverbosity(3)
                case "-vvvv":
                    increaseverbosity(4)
                default:
                    if strings.HasPrefix(arg, "-") {
                        fmt.Printf("Unknown argument: %s\n", arg)
                        os.Exit(1)
                    }else{
                        fmt.Println("Ending argument loop")
                        subcmd := args[i:] // this is what we will pass on 
                        break outer
                    }
        }
    }
}

func increaseverbosity(n int) {
    value, ok := os.LookupEnv("SINGULARITY_MESSAGELEVEL")
    if ! ok {
        value = "0"
    }
    msg_lv, err := strconv.Atoi(value);
    if err != nil {
        fmt.Println("ERROR: cannot convert $SINGULARITY_MESSAGELEVEL to integer")
        os.Exit(1)
    }
    os.Setenv("SINGULARITY_MESSAGELEVEL", strconv.Itoa(msg_lv + n))
    fmt.Printf("Increasing verbosity level (%s)\n", os.Getenv("SINGULARITY_MESSAGELEVEL"))
}
