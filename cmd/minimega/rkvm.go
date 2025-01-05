// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sandia-minimega/minimega/v2/internal/ron"
	"github.com/sandia-minimega/minimega/v2/internal/vnc"
	log "github.com/sandia-minimega/minimega/v2/pkg/minilog"
)

type ServerInfo struct {
	Width  uint16
	Height uint16
}

type RKVMConfig struct {
	Vnc_host string
	Vnc_port int
}

type RKvmVM struct {
	*BaseVM                  // embed
	RKVMConfig               // embed
	vncShim     net.Listener // shim for VNC connections
	vncShimPort int
}

// Ensure that KvmVM implements the VM interface
var _ VM = (*RKvmVM)(nil)
var killVnc = make(chan bool, 1)
var screenshot = make(chan string, 1)
var lastScreenshot []byte = []byte{}

// Copy makes a deep copy and returns reference to the new struct.
func (old RKVMConfig) Copy() RKVMConfig {
	// Copy all fields
	res := old
	return res
}

func NewRKVM(name, namespace string, config VMConfig) (*RKvmVM, error) {
	vm := new(RKvmVM)

	vm.BaseVM = NewBaseVM(name, namespace, config)
	vm.Type = RKVM

	vm.RKVMConfig = config.RKVMConfig.Copy() // deep-copy configured fields

	return vm, nil
}

func (vm *RKvmVM) Copy() VM {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	vm2 := new(RKvmVM)

	// Make shallow copies of all fields
	*vm2 = *vm

	// Make deep copies
	vm2.BaseVM = vm.BaseVM.copy()
	vm2.RKVMConfig = vm.RKVMConfig.Copy()

	return vm2
}

// Launch a new KVM VM.
func (vm *RKvmVM) Launch() error {
	defer vm.lock.Unlock()
	return vm.launch()
}

// Recover an existing KVM VM. This resets the ID and PID for the VM and unlocks
// the VM (since VMs come pre-locked when created via NewBaseVM).
func (vm *RKvmVM) Recover(id string, pid int) error {
	// reset ID and PID to match running QEMU KVM config
	vm.ID, _ = strconv.Atoi(id)

	vm.lock.Unlock()
	return nil
}

// Flush cleans up all resources allocated to the VM which includes all the
// network taps.
func (vm *RKvmVM) Flush() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()
	return vm.BaseVM.Flush()
}

func (vm *RKvmVM) Config() *BaseConfig {
	return &vm.BaseConfig
}

func (vm *RKvmVM) Start() (err error) {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.State&VM_RUNNING != 0 {
		return nil
	}

	if vm.State == VM_QUIT || vm.State == VM_ERROR {
		log.Info("relaunching VM: %v", vm.ID)

		// Create a new channel since we closed the other one to indicate that
		// the VM should quit.
		vm.kill = make(chan bool)

		// Launch handles setting the VM to error state
		if err := vm.launch(); err != nil {
			return err
		}
	}

	log.Info("starting VM: %v", vm.ID)
	/*	if err := vm.q.Start(); err != nil {
			return vm.setErrorf("unable to start: %v", err)
		}
	*/
	vm.setState(VM_RUNNING)

	return nil
}

func (vm *RKvmVM) Stop() error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.Name == "vince" {
		return errors.New("vince is not stoppable, however if one posses a rare whiskey he may be paused")
	}

	if vm.State != VM_RUNNING {
		return vmNotRunning(strconv.Itoa(vm.ID))
	}

	log.Info("stopping VM: %v", vm.ID)
	/*	if err := vm.q.Stop(); err != nil {
			return vm.setErrorf("unstoppable: %v", vm.ID)
		}
	*/
	vm.setState(VM_PAUSED)

	return nil
}

func (vm *RKvmVM) String() string {
	return fmt.Sprintf("%s:%d:rkvm", hostname, vm.ID)
}

func (vm *RKvmVM) Info(field string) (string, error) {
	// If the field is handled by BaseVM, return it
	if v, err := vm.BaseVM.Info(field); err == nil {
		return v, nil
	}

	vm.lock.Lock()
	defer vm.lock.Unlock()

	switch field {
	case "vnc_port":
		return strconv.Itoa(vm.vncShimPort), nil
	case "host":
		return vm.Vnc_host, nil
	}

	return vm.RKVMConfig.Info(field)
}

func (vm *RKvmVM) Conflicts(vm2 VM) error {
	switch vm2 := vm2.(type) {
	case *KvmVM:
		return vm.BaseVM.conflicts(vm2.BaseVM)
	case *ContainerVM:
		return vm.BaseVM.conflicts(vm2.BaseVM)
	case *RKvmVM:
		return vm.ConflictsRKVM(vm2)
	}

	return errors.New("unknown VM type")
}

// ConflictsRKVM tests whether vm and vm2 share a disk and returns an
// error if one of them is not running in snapshot mode. Also checks
// whether the BaseVMs conflict.
func (vm *RKvmVM) ConflictsRKVM(vm2 *RKvmVM) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.Vnc_host == vm2.Vnc_host {
		return fmt.Errorf("duplicate rkvm %v: %v", vm.Name, vm2.Name)
	}
	return vm.BaseVM.conflicts(vm2.BaseVM)
}

func (vm *RKVMConfig) String() string {
	// create output
	var o bytes.Buffer
	w := new(tabwriter.Writer)
	w.Init(&o, 5, 0, 1, ' ', 0)
	fmt.Fprintln(&o, "RKVM configuration:")
	fmt.Fprintf(w, "Vnc Host:\t%v\n", vm.Vnc_host)
	fmt.Fprintf(w, "Vnc Port:\t%v\n", vm.Vnc_port)
	w.Flush()
	fmt.Fprintln(&o)
	return o.String()
}

func (vm *RKvmVM) Screenshot(size int) ([]byte, error) {
	if vm.GetState()&VM_RUNNING == 0 {
		return nil, vmNotRunning(strconv.Itoa(vm.ID))
	}

	//	suffix := rand.New(rand.NewSource(time.Now().UnixNano())).Int31()
	//	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("minimega_screenshot_%v", suffix))
	tmp := ""
	select {
	case tmp = <-screenshot:
		ppmFile, err := ioutil.ReadFile(tmp)
		if err != nil {
			return nil, err
		}

		pngResult, err := ppmToPng(ppmFile, size)
		if err != nil {
			return nil, err
		}
		lastScreenshot = pngResult
		log.Debug("Reading Screenshot: %v", tmp)
		return pngResult, nil

	default:
		return lastScreenshot, nil

	}

}

func (vm *RKvmVM) connectVNC(config VMConfig) error {
	// should never create...
	ns := GetOrCreateNamespace(vm.Namespace)
	connectionString := config.RKVMConfig.Vnc_host + ":" + strconv.Itoa(config.RKVMConfig.Vnc_port)

	shim, shimErr := net.Listen("tcp", "")
	if shimErr != nil {
		log.Error("Shim listen error on %v", vm.Name)
		return shimErr
	}
	vm.vncShim = shim
	vm.vncShimPort = shim.Addr().(*net.TCPAddr).Port
	log.Debug("Server at :" + strconv.Itoa(vm.vncShimPort) + "for " + vm.Name)
	//var vncRx_c = make(chan interface{}, 1)
	var vncTx_c = make(chan interface{}, 4)
	var serverIntInfo_c = make(chan ServerInfo, 1)
	var killFbRes_c = make(chan bool, 1)
	var killFbReq_c = make(chan bool, 1)
	//var killVncRx_c = make(chan bool, 1)
	var killShim_c = make(chan bool, 1)
	var killShimAccept_c = make(chan bool, 1)
	var resetCon_c = make(chan bool, 1)
	//var conError_c = make(chan error, 1)

	go func() {
		defer shim.Close()
		var remote net.Conn
		for {
			select {
			case <-killShimAccept_c:
				log.Debug("Kill Shim Listen %v", vm.Name)
				return
			default:
			}
			log.Info("vnc shim listen: %v", vm.Name)
			connection := make(chan net.Conn, 1)
			go func() {
				conn, err := shim.Accept()
				if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
					return
				} else if err != nil {
					log.Errorln(err)
					return
				}
				connection <- conn
				return
			}()
			select {
			case remote = <-connection:
				log.Info("Vnc shim connect: %v -> %v", remote.RemoteAddr(), vm.Name)

				//go func() {
				defer remote.Close()
				// Dial domain socket
				local, err := net.Dial("tcp", connectionString)
				log.Info("Dialing VNC: %v for %v", connectionString, vm.Name)
				if err != nil {
					log.Error("unable to dial vm vnc: %v", err)
					return
				}
				defer local.Close()
				// copy local -> remote
				go io.Copy(remote, local)
				// Reads will implicitly copy from remote -> local
				tee := io.TeeReader(remote, local)
				for {

					msg, err := vnc.ReadClientMessage(tee)
					if err == nil {
						ns.Recorder.Route(vm.GetName(), msg)
						continue
					}
					// shim is no longer connected
					if err == io.EOF || strings.Contains(err.Error(), "broken pipe") {
						log.Info("Vnc shim quit: %v", vm.Name)
						break
					}
					// ignore these
					if strings.Contains(err.Error(), "unknown client-to-server message") {
						continue
					}
					//unknown error
					log.Warnln(err)
					select {
					case <-killShim_c:
						killShimAccept_c <- true
						return
					default:
					}
				}
			case <-killShim_c:
				log.Debug("Kill Shim for %v", vm.Name)
				killShimAccept_c <- true
				return

			}

			//}()
		}
	}()

	go func(vm *RKvmVM) {
		var sin ServerInfo
		var con *vnc.Conn
		var conErr error
		var reset bool = true
		var errorCount int = 0
		var conErrorCount int = 0
		var connected bool = false
		//var ID = vm.ID
		for {
			select {
			case reset = <-resetCon_c:
				if connected {
					killFbReq_c <- true
					killFbRes_c <- true
					//killVncRx_c <- true
					time.Sleep(2 * time.Second)
					con.Close()

				}

			case <-killVnc:
				log.Debug("Kill VNC for %v")
				//killVncRx_c <- true

				//killFbReq_c <- true
				killFbRes_c <- true
				//_, _ = vnc.Dial("127.0.0.1" + ":" + strconv.Itoa(vm.vncShimPort))
				//time.Sleep(2 * time.Second)
				//con.Close()
				killShim_c <- true
				return

			case <-vm.kill:
				//killVncRx_c <- true
				killShim_c <- true
				//_, _ = vnc.Dial("127.0.0.1" + ":" + strconv.Itoa(vm.vncShimPort))
				//time.Sleep(2 * time.Second)
				//con.Close()
				if connected {
					reset = false
					killFbReq_c <- true
					killFbRes_c <- true
					resetCon_c <- false
					//shim.Close()
					errorCount = 0
				}

				vm.setState(VM_QUIT)
				vm.cond.Signal()
				return
			case tx := <-vncTx_c:
				//fmt.Println("TX Signal Comm")
				switch tx.(type) {
				case *vnc.FramebufferUpdateRequest:
					//fmt.Println("TX FB Request")
					sender := tx.(*vnc.FramebufferUpdateRequest)
					if err := sender.Write(con); err != nil {
						log.Error("unable to request framebuffer update")
						errorCount++
					}
				default:
					log.Error("unimplemented send type in vncTx_c channel")
				}

			default:
				//fmt.Println("No signals communicating")
				//break
			} //end select for channel communication
			if errorCount > 3 {
				errorCount = 0
				resetCon_c <- true
			}

			if reset {
				//fmt.Println("RESET VNC Connection")
				reset = false
				con, conErr = vnc.Dial(connectionString)
				if conErr != nil {
					log.Error("unable to dial vm vnc %v: %v", connectionString, conErr)
					conErrorCount++
					connected = false
					resetCon_c <- true
					time.Sleep(5 * time.Second)
					//fmt.Println("RESETTING")
					continue
				}
				if conErrorCount > 12 {
					vm.setErrorf("unable to connect to vnc after 1 minute %v ", conErr)
					//fmt.Println("Exit connect loop")
					killVnc <- true
					continue
				}

				height, width := con.GetDesktopSize()
				sin.Height = height
				sin.Width = width
				serverIntInfo_c <- sin // give information to other routines
				log.Debug("Size %v x %v for %v \n", width, height, vm.Name)
				connected = true

				go func() {
					t0 := time.Now()
					for {
						//Frame Buffer Request
						t1 := time.Now()
						if t1.Sub(t0).Seconds() > 5 {
							req := &vnc.FramebufferUpdateRequest{}
							req.Width = width - 1
							req.Height = height - 1
							vncTx_c <- req
							t0 = time.Now()
						}

						//check if we need to kill
						select {
						case <-killFbReq_c:
							return
						default:
						}
					}
				}()

				//Frame Buffer Response
				go func(con *vnc.Conn) {
					var serverInfo ServerInfo
					serverInfo = <-serverIntInfo_c
					masterFrame := image.NewRGBA(image.Rectangle{image.Point{0, 0}, image.Point{int(serverInfo.Width), int(serverInfo.Height)}})

					for {

						//check if we need to kill
						select {
						case <-killFbRes_c:
							return
						default:

						}
						//fmt.Println("Attempt Vnc Read")
						msg, err := con.ReadMessage()
						//fmt.Println("VNC Message Read")
						if err == nil {
							ns.Recorder.Route(vm.GetName(), msg)
							//vncRx_c <- msg
						} else if strings.Contains(err.Error(), "unknown client-to-server message") {
							//fmt.Println("Unknown message %v",msg)
							log.Error("Unknown message from vnc server")
						} else {
							// unknown error
							log.Warnln(err)
						}
						//fmt.Println("vnc rx channel read success")
						switch msg.(type) {
						case *vnc.FramebufferUpdate:
							fb := msg.(*vnc.FramebufferUpdate)
							for _, rect := range fb.Rectangles {
								// ignore non-image
								if rect.RGBA == nil {
									continue
								}
								drawPoint := image.Point{int(rect.X), int(rect.Y)}
								draw.Draw(masterFrame, image.Rect(int(rect.X), int(rect.Y), int(rect.X+rect.Width), int(rect.Height+rect.Y)), rect.RGBA, drawPoint, draw.Src)
							} // end for rectangles
							suffix := rand.New(rand.NewSource(time.Now().UnixNano())).Int31()
							tmp := filepath.Join(os.TempDir(), fmt.Sprintf("minimega_screenshot_%v", suffix))
							out, err := os.Create(tmp)
							if err != nil {
								log.Error("Error creating screenshot file", err)
							} else {
								err = png.Encode(out, masterFrame)
								out.Close()
								if err != nil {
									log.Error("Error writing out screenshot %v", err)
								}
							}
							select {
							case screenshot <- tmp:
								log.Debug("Writing " + tmp)
							default:
							}
						default:
						}
					} // end for loop
				}(con) //end Read for Framebuffer Update
			} //end if reset
		} //end for loop
	}(vm)
	//error catch
	//	select {
	//	case er := <-conError_c:
	//		return er
	//	default:
	return nil
	//	}

}

// launch is the low-level launch function for KVM VMs. The caller should hold
// the VM's lock.
func (vm *RKvmVM) launch() error {
	log.Info("launching vm: %v", vm.ID)

	// If this is the first time launching the VM, do the final configuration
	// check and create directories for it.
	if vm.State == VM_BUILDING {
		// create a directory for the VM at the instance path
		if err := os.MkdirAll(vm.instancePath, os.FileMode(0700)); err != nil {
			return vm.setErrorf("unable to create VM dir: %v", err)
		}

	}
	mustWrite(vm.path("name"), vm.Name)

	vmConfig := VMConfig{BaseConfig: vm.BaseConfig, RKVMConfig: vm.RKVMConfig}

	// if using bidirectionalCopyPaste, error out if dependencies aren't met
	//	if vmConfig.BidirectionalCopyPaste {
	//		if err := checkVersion("qemu", MIN_QEMU_COPY_PASTE, qemuVersion); err != nil {
	//			return fmt.Errorf("bidirectional-copy-paste not supported. Please disable: %v", err)
	//		}
	//		if err := checkQemuChardev("qemu-vdagent"); err != nil {
	//			return fmt.Errorf("bidirectional-copy-paste not supported. Please disable: %v", err)
	//		}
	//	}

	if err := vm.createInstancePathAlias(); err != nil {
		return vm.setErrorf("createInstancePathAlias: %v", err)
	}

	err := vm.connectVNC(vmConfig)

	return err

}

/*
func (vm *RKvmVM) waitForExit(p *os.Process, wait chan bool) {
	// Create goroutine to wait for process to exit
	go func() {
		defer close(wait)
		_, err := p.Wait()

		if err != nil && err.Error() == "waitid: no child processes" {
			for {
				time.Sleep(1 * time.Second)

				if err = p.Signal(syscall.Signal(0)); err != nil {
					break
				}
			}
		}

		vm.lock.Lock()
		defer vm.lock.Unlock()

		// Check if the process quit for some reason other than being killed
		if err != nil && err.Error() != "signal: killed" {
			vm.setErrorf("qemu killed: %v", err)
		} else if vm.State != VM_ERROR {
			// Set to QUIT unless we've already been put into the error state
			vm.setState(VM_QUIT)
		}

		// Kill the VNC shim, if it exists
		if vm.vncShim != nil {
			vm.vncShim.Close()
		}
	}()
}

func (vm *RKvmVM) waitToKill(p *os.Process, wait chan bool) {
	// Create goroutine to wait to kill the VM
	go func() {
		defer vm.cond.Signal()

		select {
		case <-wait:
			log.Info("VM %v exited", vm.ID)
		case <-vm.kill:
			log.Info("Killing VM %v", vm.ID)
			p.Kill()
			<-wait
		}
	}()
}
*/
func (vm *RKvmVM) Connect(cc *ron.Server, reconnect bool) error {
	if !vm.Backchannel {
		return nil
	}

	if !reconnect {
		cc.RegisterVM(vm)
	}
	return nil
}

func (vm *RKvmVM) Disconnect(cc *ron.Server) error {
	if !vm.Backchannel {
		return nil
	}

	cc.UnregisterVM(vm)

	return nil
}

/*
func (vm *RKvmVM) ChangeCD(f string, force bool) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.cdromPath != "" {
		if err := vm.ejectCD(force); err != nil {
			return err
		}
	}
	if err == nil {
		vm.cdromPath = f
	}

	return err
}

func (vm *RKvmVM) EjectCD(force bool) error {
	vm.lock.Lock()
	defer vm.lock.Unlock()

	if vm.cdromPath == "" {
		return errors.New("no cdrom inserted")
	}

	return vm.ejectCD(force)
}

func (vm *RKvmVM) ejectCD(force bool) error {
	//err := vm.q.BlockdevEject("ide0-cd0", force)
	if err == nil {
		vm.cdromPath = ""
	}

	return err
}
*/
func (vm *RKvmVM) ProcStats() (map[int]*ProcStats, error) {
	return nil, nil
}

func (vm *RKvmVM) WriteConfig(w io.Writer) error {
	if err := vm.BaseConfig.WriteConfig(w); err != nil {
		return err
	}

	return vm.RKVMConfig.WriteConfig(w)
}
