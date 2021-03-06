// hid - Gopher Interface Devices (USB HID)
// Copyright (c) 2017 Péter Szilágyi. All rights reserved.
//
// This file is released under the 3-clause BSD license. Note however that Linux
// support depends on libusb, released under LGNU GPL 2.1 or later.

// +build linux,cgo darwin,!ios,cgo windows,cgo

package hid

/*
#cgo linux CFLAGS: -DDEFAULT_VISIBILITY="" -DOS_LINUX -D_GNU_SOURCE -DPOLL_NFDS_TYPE=int
#cgo linux,!android LDFLAGS: -lrt -lhidapi-hidraw
#cgo darwin CFLAGS: -DOS_DARWIN
#cgo darwin LDFLAGS: -framework CoreFoundation -framework IOKit
#cgo windows CFLAGS: -DOS_WINDOWS
#cgo windows LDFLAGS: -lsetupapi

#ifdef OS_LINUX
	#include <sys/poll.h>
	#include <hidapi/hidapi.h>
	#include <malloc.h>
#endif

#include <errno.h>
int GoHidGetErrno() {
	return errno;
}

*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// enumerateLock is a mutex serializing access to USB device enumeration needed
// by the macOS USB HID system calls, which require 2 consecutive method calls
// for enumeration, causing crashes if called concurrently.
//
// For more details, see:
//   https://developer.apple.com/documentation/iokit/1438371-iohidmanagersetdevicematching
//   > "subsequent calls will cause the hid manager to release previously enumerated devices"
var enumerateLock sync.Mutex

func Init() error {
	if res := C.hid_init(); res < 0 {
		return fmt.Errorf("Failed to initialize hidapi: %v", res)
	}
	return nil
}

func Shutdown() error {
	if res := C.hid_exit(); res < 0 {
		return fmt.Errorf("Failed to clean up hidapi: %v", res)
	}
	return nil
}

// Supported returns whether this platform is supported by the HID library or not.
// The goal of this method is to allow programatically handling platforms that do
// not support USB HID and not having to fall back to build constraints.
func Supported() bool {
	return true
}

// Enumerate returns a list of all the HID devices attached to the system which
// match the vendor and product id:
//  - If the vendor id is set to 0 then any vendor matches.
//  - If the product id is set to 0 then any product matches.
//  - If the vendor and product id are both 0, all HID devices are returned.
func Enumerate(vendorID uint16, productID uint16) []DeviceInfo {
	enumerateLock.Lock()
	defer enumerateLock.Unlock()

	// Gather all device infos and ensure they are freed before returning
	head := C.hid_enumerate(C.ushort(vendorID), C.ushort(productID))
	if head == nil {
		return nil
	}
	defer C.hid_free_enumeration(head)

	// Iterate the list and retrieve the device details
	var infos []DeviceInfo
	for ; head != nil; head = head.next {
		info := DeviceInfo{
			Path:      C.GoString(head.path),
			VendorID:  uint16(head.vendor_id),
			ProductID: uint16(head.product_id),
			Release:   uint16(head.release_number),
			UsagePage: uint16(head.usage_page),
			Usage:     uint16(head.usage),
			Interface: int(head.interface_number),
		}
		if head.serial_number != nil {
			info.Serial, _ = wcharTToString(head.serial_number)
		}
		if head.product_string != nil {
			info.Product, _ = wcharTToString(head.product_string)
		}
		if head.manufacturer_string != nil {
			info.Manufacturer, _ = wcharTToString(head.manufacturer_string)
		}
		infos = append(infos, info)
	}
	return infos
}

// Open connects to an HID device by its path name.
func (info DeviceInfo) Open() (*Device, error) {
	path := C.CString(info.Path)
	defer C.free(unsafe.Pointer(path))

	device := C.hid_open_path(path)
	if device == nil {
		return nil, errors.New("hidapi: failed to open device")
	}
	return &Device{
		DeviceInfo: info,
		device:     device,
	}, nil
}

// Device is a live HID USB connected device handle.
type Device struct {
	DeviceInfo               // Embed the infos for easier access
	device     *C.hid_device // Low level HID device to communicate through
}

// Close releases the HID USB device handle.
func (dev *Device) Close() error {
	if dev.device != nil {
		C.hid_close(dev.device)
		dev.device = nil
	}
	return nil
}

func (dev *Device) Write(b []byte) (int, error) {
	return dev.DoWrite(b, false)
}

func (dev *Device) Read(b []byte) (int, error) {
	return dev.DoRead(b, false, 0)
}

// Write sends an output report to a HID device.
//
// Write will send the data on the first OUT endpoint, if one exists. If it does
// not, it will send the data through the Control Endpoint (Endpoint 0).
func (dev *Device) DoWrite(b []byte, featureReport bool) (int, error) {
	// Abort if nothing to write
	if len(b) == 0 {
		return 0, nil
	}
	// Abort if device closed in between
	device := dev.device
	if device == nil {
		return 0, ErrDeviceClosed
	}
	// Prepend a HID report ID on Windows, other OSes don't need it
	var report []byte
	if runtime.GOOS == "windows" {
		report = append([]byte{0x00}, b...)
	} else {
		report = b
	}
	// Execute the write operation
	var written int
	if featureReport {
		written = int(C.hid_send_feature_report(device, (*C.uchar)(&report[0]), C.size_t(len(report))))
	} else {
		written = int(C.hid_write(device, (*C.uchar)(&report[0]), C.size_t(len(report))))
	}
	if written == -1 {
		return 0, dev.getError()
	}
	return written, nil
}

// Read retrieves an input report from a HID device.
func (dev *Device) DoRead(b []byte, featureReport bool, timeout time.Duration) (int, error) {
	// Abort if nothing to read
	if len(b) == 0 {
		return 0, nil
	}

	// Abort if device closed in between
	device := dev.device
	if device == nil {
		return 0, ErrDeviceClosed
	}
	// Execute the read operation
	var read int
	if featureReport {
		read = int(C.hid_get_feature_report(device, (*C.uchar)(&b[0]), C.size_t(len(b))))
	} else {
		if timeout > 0 {
			read = int(C.hid_read_timeout(device, (*C.uchar)(&b[0]), C.size_t(len(b)), C.int(timeout/time.Millisecond)))
			if read == 0 {

			}
		} else {
			read = int(C.hid_read(device, (*C.uchar)(&b[0]), C.size_t(len(b))))
		}
	}

	if read == -1 {
		return 0, dev.getError()
	}
	return read, nil
}

func (dev *Device) getError() error {
	// If the operation failed, verify if closed or other error
	if dev == nil {
		return ErrDeviceClosed
	}
	// Device not closed, some other error occurred
	message := C.hid_error(dev.device)
	if message == nil {
		cErrno := C.GoHidGetErrno() // Defined at import "C"
		err := syscall.Errno(cErrno)
		return fmt.Errorf("hidapi: unknown failure. Errno: %v", err)
	}
	failure, _ := wcharTToString(message)
	return errors.New("hidapi: " + failure)
}
