// Copyright (2014) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"fmt"
	log "minilog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"vnc"
)

type vncClient struct {
	VM    *KvmVM
	ID    string
	Rhost string

	err error

	Conn *vnc.Conn
	file *os.File

	start time.Time
	done  chan bool
}

// NewVNCClient creates a new VNC client. NewVNCClient is only called via the
// cli so we can assume that cmdLock is held.
// This is sent via wrapVMTargetCLI so we assume the command will always be
// delivered to the correct host
func NewVNCClient(vm *KvmVM) (*vncClient, error) {
	rhost := fmt.Sprintf("%v:%v", hostname, vm.VNCPort)
	id := fmt.Sprintf("%v:%v", vm.Namespace, vm.Name)

	c := &vncClient{
		ID:    id, // ID is namespace:name
		Rhost: rhost,
		start: time.Now(),
		VM:    vm,
		done:  make(chan bool),
	}

	return c, nil
}

func (v *vncClient) Matches(host, vm string) bool {
	return v.VM.Host == host && v.VM.Name == vm
}

func (v *vncClient) Stop() error {

	if v.file != nil {
		v.file.Close()
	}

	if v.Conn != nil {
		v.Conn.Close()
	}

	return v.err
}

// RecordMessage records a VNC client-to-server message in plaintext
func (r *vncKBRecord) RecordMessage(msg interface{}) {
	delta := time.Now().Sub(r.last).Nanoseconds()

	switch msg := msg.(type) {
	case *vnc.SetPixelFormat:
	case *vnc.SetEncodings:
	case *vnc.FramebufferUpdateRequest:
	case *vnc.ClientCutText:
		// Don't record
	case *vnc.KeyEvent, *vnc.PointerEvent:
		fmt.Fprintf(r.file, "%d:%s\n", delta, msg)
		r.last = time.Now()
	default:
		log.Info("unexpected VNC client-to-server message: %#v\n", msg)
	}
}

func (r *vncKBRecord) Run() {
	<-r.done
}

func (v *vncFBRecord) Run() {
	err := (&vnc.SetPixelFormat{
		PixelFormat: vnc.PixelFormat{
			BitsPerPixel: 32, Depth: 24, TrueColorFlag: 1,
			RedMax: 255, GreenMax: 255, BlueMax: 255,
			RedShift: 16, GreenShift: 8, BlueShift: 0,
		},
	}).Write(v.Conn)

	if err != nil {
		v.err = fmt.Errorf("unable to set pixel format: %v", err)
		return
	}

	err = (&vnc.SetEncodings{
		Encodings: []int32{vnc.RawEncoding, vnc.DesktopSizePseudoEncoding},
	}).Write(v.Conn)

	if err != nil {
		v.err = fmt.Errorf("unable to set encodings: %v", err)
		return
	}

	go func() {
		prev := time.Now()
		buf := make([]byte, 4096)
		writer := gzip.NewWriter(v.file)
		defer writer.Close()

		for {
			n, err := v.Conn.Read(buf)
			if err != nil {
				log.Debug("vnc fb response read failed: %v", err)
				break
			}

			if n > 0 {
				offset := time.Now().Sub(prev).Nanoseconds()
				header := fmt.Sprintf("%d %d\r\n", offset, n)

				if _, err := io.WriteString(writer, header); err != nil {
					log.Debug("vnc fb write chunk header failed: %v", err)
					break
				}
				if _, err := writer.Write(buf[:n]); err != nil {
					log.Debug("vnc fb write chunk failed: %v", err)
					break
				}
				if _, err := io.WriteString(writer, "\r\n"); err != nil {
					log.Debug("vnc fb write chunk tailer failed: %v", err)
					break
				}

				prev = time.Now()

				log.Debug("vnc fb wrote %d bytes", n)
			}
		}
	}()

	req := &vnc.FramebufferUpdateRequest{}

	// fall into a loop issuing periodic fb update requests and dump to file
	// check if we need to quit
	for {
		select {
		case <-v.done:
			break
		default:
		}

		if err := req.Write(v.Conn); err != nil {
			v.err = errors.New("unable to request framebuffer update")
			return
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func ParseLoadFileEvent(arg string) (string, error) {
	var filename string
	format := "LoadFile,%s"
	_, err := fmt.Sscanf(arg, format, &filename)
	if err != nil {
		return "", err
	}

	return filename, nil
}

func (v *vncKBPlayback) playFile() {
	scanner := bufio.NewScanner(v.file)

	for scanner.Scan() && v.err == nil {
		s := strings.SplitN(scanner.Text(), ":", 2)

		duration, err := time.ParseDuration(s[0] + "ns")
		if err != nil {
			log.Errorln(err)
			continue
		}

		wait := time.After(duration)
		select {
		case <-wait:
		case <-v.done:
			return
		}

		if res, err := vnc.ParseKeyEvent(s[1]); err == nil {
			v.err = res.Write(v.Conn)
		} else if res, err := vnc.ParsePointerEvent(s[1]); err == nil {
			v.err = res.Write(v.Conn)
		} else if file, err := ParseLoadFileEvent(s[1]); err == nil {
			if !filepath.IsAbs(file) {
				// Our file is in the same directory as the parent
				file = filepath.Join(filepath.Dir(v.file.Name()), file)
			}
			// Save the file we were working from
			oldfile := v.file
			// Load the new file
			v.file, err = os.Open(file)
			if err != nil {
				log.Error("Couldn't load VNC playback file %v: %v", file, err)
				v.err = err
			} else {
				r := &vncKBPlayback{v.vncClient}
				// We will wait until this file has played fully.
				r.playFile()
			}
			v.file = oldfile
		} else {
			log.Error("invalid vnc message: `%s`", s[1])
		}
	}
}

func (v *vncKBPlayback) Run() {
	err := (&vnc.SetEncodings{
		Encodings: []int32{vnc.CursorPseudoEncoding},
	}).Write(v.Conn)

	if err != nil {
		v.err = fmt.Errorf("unable to set encodings: %v", err)
		return
	}

	v.playFile()

	// Stop ourselves
	go v.Stop()

	// Block until we receive the done flag if we finished the playback
	<-v.done
	delete(vncKBPlaying, v.ID)
}

func vncRecordKB(vm *KvmVM, filename string) error {
	c, err := NewVNCClient(vm)
	if err != nil {
		return err
	}

	// is this namespace:vm already being recorded?
	if _, ok := vncKBRecording[c.ID]; ok {
		return fmt.Errorf("kb recording for %v already running", vm.Name)
	}

	c.file, err = os.Create(filename)
	if err != nil {
		return err
	}

	r := &vncKBRecord{vncClient: c, last: time.Now()}

	// Recordings are stored in the format namespace:vm
	vncKBRecording[c.ID] = r

	go r.Run()

	return nil
}

func vncRecordFB(vm *KvmVM, filename string) error {
	c, err := NewVNCClient(vm)
	if err != nil {
		return err
	}

	// is this namespace:vm already being recorded?
	if _, ok := vncFBRecording[c.ID]; ok {
		return fmt.Errorf("fb recording for %v already running", vm.Name)
	}

	c.file, err = os.Create(filename)
	if err != nil {
		return err
	}

	c.Conn, err = vnc.Dial(c.Rhost)
	if err != nil {
		return err
	}

	r := &vncFBRecord{c}
	vncFBRecording[c.ID] = r

	go r.Run()

	return nil
}

func vncPlaybackKB(vm *KvmVM, filename string) error {
	c, err := NewVNCClient(vm)
	if err != nil {
		return err
	}

	// is this rhost already being recorded?
	if _, ok := vncKBPlaying[c.ID]; ok {
		return fmt.Errorf("kb playback for %v already running", vm.Name)
	}

	c.file, err = os.Open(filename)
	if err != nil {
		return err
	}

	c.Conn, err = vnc.Dial(c.Rhost)
	if err != nil {
		return err
	}

	r := &vncKBPlayback{c}
	vncKBPlaying[c.ID] = r

	go r.Run()

	return nil
}

func vncClear() {
	for k, v := range vncKBRecording {
		if inNamespace(v.VM) {
			log.Debug("stopping kb recording for %v", k)
			if err := v.Stop(); err != nil {
				log.Error("%v", err)
			}

			delete(vncKBRecording, k)
		}
	}

	for k, v := range vncFBRecording {
		if inNamespace(v.VM) {
			log.Debug("stopping fb recording for %v", k)
			if err := v.Stop(); err != nil {
				log.Error("%v", err)
			}

			delete(vncFBRecording, k)
		}
	}

	for k, v := range vncKBPlaying {
		if inNamespace(v.VM) {
			log.Debug("stopping kb playing for %v", k)
			if err := v.Stop(); err != nil {
				log.Error("%v", err)
			}

			delete(vncKBPlaying, k)
		}
	}
}
