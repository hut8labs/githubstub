package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"
)

var (
	address       = flag.String("address", ":8000", "Address to list on")
	secretFile    = flag.String("secret-file", "", "File containing github secret for authenticating requests (incompatible with --no-secret)")
	tmpDir        = flag.String("tmp-dir", "/tmp", "Directory to use for temporary files")
	workingDir    = flag.String("working-dir", "", "Change to this directory before running command")
	timeoutInSecs = flag.Int("timeout-in-secs", 60, "Timeout (in seconds) for the command")
	noSecret      = flag.Bool("no-secret", false, "Tolerate the presence of X-Hub-Signature without a local secret (without this, githubstub will refuse to accept signed hooks without a secret)")
	command       string
	commandArgs   []string
)

func validateHMAC(secret []byte, body []byte, sig []byte) bool {
	mac := hmac.New(sha1.New, secret)
	mac.Write(body)
	computedSig := []byte(fmt.Sprintf("sha1=%x", mac.Sum(nil)))
	return subtle.ConstantTimeCompare(computedSig, sig) == 1
}

func readBody(req *http.Request) ([]byte, error) {
	// We'll assume that anything over 5MB probably isn't valid.
	maxBodySize := int64(1024 * 1024 * 5)
	bodyBytes, err := ioutil.ReadAll(io.LimitReader(req.Body, maxBodySize+1))

	if err != nil {
		return nil, err
	}

	bodySize := int64(len(bodyBytes))

	if bodySize > maxBodySize {
		return nil, fmt.Errorf("Body size exceeded max allowed of %d", maxBodySize)
	}

	return bodyBytes, nil
}

func handleReqWithSecret(secret []byte, w http.ResponseWriter, req *http.Request) {
	body, err := readBody(req)

	if err != nil {
		log.Printf("Error reading body %s", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	sig := req.Header.Get("X-Hub-Signature")

	if sig == "" {
		log.Printf("Error: secret provided but no X-Hub-Signature, refusing to run command")
		http.Error(w, "Not authorized.", http.StatusUnauthorized)
		return
	}

	if !validateHMAC(secret, body, []byte(sig)) {
		log.Printf("Error: Body did not validate with HMAC, refusing to run command")
		http.Error(w, "Not authorized.", http.StatusUnauthorized)
		return
	}

	err = runCommand(w, body)
	if err != nil {
		log.Printf("Error running command %s", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}

func handleReqWithoutSecret(w http.ResponseWriter, req *http.Request) {
	sig := req.Header.Get("X-Hub-Signature")

	if sig != "" && !*noSecret {
		log.Printf("Error: Signed hook but no local secret, refusing to run command (did you mean to run with --secret-file or --no-secret?)")
		http.Error(w, "Not authorized.", http.StatusUnauthorized)
		return
	}

	body, err := readBody(req)
	if err != nil {
		log.Printf("Error reading body %s", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	err = runCommand(w, body)
	if err != nil {
		log.Printf("ERROR running command %s", err)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}

func runCommand(w http.ResponseWriter, body []byte) error {
	cmdStdout, err := ioutil.TempFile(*tmpDir, "githubstub")
	if err != nil {
		return err
	}
	defer cmdStdout.Close()

	cmdStderr, err := ioutil.TempFile(*tmpDir, "githubstub")
	if err != nil {
		return err
	}
	defer cmdStderr.Close()

	cmd := exec.Command(command, commandArgs...)
	cmd.Stdout = cmdStdout
	cmd.Stderr = cmdStderr
	cmd.Stdin = bytes.NewBuffer(body)

	if *workingDir != "" {
		cmd.Dir = *workingDir
	}

	cmdDone := make(chan error)

	go func() {
		cmdDone <- cmd.Run()
	}()

	err = waitForCmd(cmd, *timeoutInSecs, cmdDone)

	if err != nil {
		logFailingStdoutAndErr(cmdStdout, cmdStderr)
	}

	return err
}

func logFailingStdoutAndErr(cmdStdout, cmdStderr *os.File) {
	cmdStdout.Seek(0, 0)
	cmdStderr.Seek(0, 0)

	var buf bytes.Buffer

	io.Copy(&buf, cmdStdout)
	log.Printf("Error running command, dumping stdout: %s", buf.String())

	buf.Reset()

	io.Copy(&buf, cmdStderr)
	log.Printf("Error running command, dumping stderr: %s", buf.String())
}

func waitForCmd(cmd *exec.Cmd, timeoutInSecs int, cmdDone <-chan error) error {
	var err error

	select {
	case result := <-cmdDone:
		err = result
	case <-time.After(time.Second * time.Duration(timeoutInSecs)):
		err = fmt.Errorf("Command timed out after %d seconds", timeoutInSecs)
		if perr := cmd.Process.Kill(); perr != nil {
			// TODO: try -9'ing with signal
			//
			// TODO: reconsider fatal--feels better at first blush than
			// allowing long-running tasks to accumulate, but...
			log.Fatalf("Could not kill long running process, aborting in dirty state: %s", perr)
		}
		<-cmdDone
	}

	return err
}

func createHandleReq(secretFile *string) func(w http.ResponseWriter, req *http.Request) {
	secret := []byte{}
	var err error

	if *secretFile != "" {
		secret, err = ioutil.ReadFile(*secretFile)
		if err != nil {
			log.Fatalf("Error reading secret file %s %s", *secretFile, err)
		}
		if len(secret) == 0 {
			log.Fatalf("Secret file %s was empty", *secretFile)
		}
	}

	return func(w http.ResponseWriter, req *http.Request) {
		log.Printf("Handling %v %v", req.Method, req.URL.Path)

		switch req.Method {
		case "POST":
			if len(secret) != 0 {
				handleReqWithSecret(secret, w, req)
			} else {
				handleReqWithoutSecret(w, req)
			}
		default:
			http.Error(w, "Only GET and POST supported", http.StatusMethodNotAllowed)
		}
	}
}

func main() {
	// TODO support ssl
	flag.Usage = func() {
		fmt.Printf("Usage: githubstub [options] <cmd to run on hook>\n")
		flag.PrintDefaults()
		fmt.Printf("\n\nListens for Github webhook pushes and executes the supplied command on arrival, pushing the headers and body to its stdin.\n\n")
		fmt.Printf("The command is executed with arguments as parsed on the command line.\n\n")
		fmt.Printf("Example:\n\n")
		fmt.Printf("githubstub '/home/user/bin/mail_notifications.py' 'someone@example.com'\n")
	}

	flag.Parse()

	if len(flag.Args()) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	command = flag.Arg(0)
	commandArgs = flag.Args()[1:]

	if *secretFile != "" && *noSecret {
		fmt.Printf("Error: --no-secret and --secret-file are incompatible\n\n")
		flag.Usage()
		os.Exit(1)
	}

	http.HandleFunc("/", createHandleReq(secretFile))
	http.HandleFunc("/favicon.ico",
		func(w http.ResponseWriter, req *http.Request) {
			http.Error(w, "No favicon", http.StatusGone)
		})

	log.Fatal(http.ListenAndServe(*address, nil))
}
