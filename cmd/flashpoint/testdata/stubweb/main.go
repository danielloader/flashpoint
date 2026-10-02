// Command stubweb stands in for Vite in the end-to-end tests: it serves on
// the port it is given and spawns a child, as npm does node.
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) > 2 && os.Args[1] == "child" {
		fmt.Println("stub child", os.Getpid())
		http.ListenAndServe("localhost:"+os.Args[2], http.NotFoundHandler())
		return
	}
	child := exec.Command(os.Args[0], "child", os.Args[1])
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		panic(err)
	}
	fmt.Println("stub web ready, api at", os.Getenv("FLASHPOINT_API_URL"))
	child.Wait()
}
