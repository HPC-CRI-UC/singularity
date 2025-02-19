// Copyright (c) 2018-2021, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package assemblers

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"syscall"

	"github.com/hpcng/sif/pkg/sif"
	"github.com/hpcng/singularity/internal/pkg/util/crypt"
	"github.com/hpcng/singularity/internal/pkg/util/machine"
	"github.com/hpcng/singularity/pkg/build/types"
	"github.com/hpcng/singularity/pkg/image/packer"
	"github.com/hpcng/singularity/pkg/sylog"
	"github.com/hpcng/singularity/pkg/util/cryptkey"
	uuid "github.com/satori/go.uuid"
)

// SIFAssembler doesn't store anything.
type SIFAssembler struct {
	GzipFlag        bool
	MksquashfsProcs uint
	MksquashfsMem   string
	MksquashfsPath  string
}

type encryptionOptions struct {
	keyInfo   cryptkey.KeyInfo
	plaintext []byte
}

func createSIF(path string, b *types.Bundle, squashfile string, encOpts *encryptionOptions, arch string) (err error) {
	definition := b.Recipe.Raw

	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("sif id generation failed: %v", err)
	}

	// general info for the new SIF file creation
	cinfo := sif.CreateInfo{
		Pathname:   path,
		Launchstr:  sif.HdrLaunch,
		Sifversion: sif.HdrVersion,
		ID:         id,
	}

	// data we need to create a definition file descriptor
	definput := sif.DescriptorInput{
		Datatype: sif.DataDeffile,
		Groupid:  sif.DescrDefaultGroup,
		Link:     sif.DescrUnusedLink,
		Data:     definition,
	}
	definput.Size = int64(binary.Size(definput.Data))

	// add this descriptor input element to creation descriptor slice
	cinfo.InputDescr = append(cinfo.InputDescr, definput)

	// add all JSON data object within SIF by alphabetical order
	sorted := make([]string, 0, len(b.JSONObjects))
	for name := range b.JSONObjects {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		if len(b.JSONObjects[name]) > 0 {
			// data we need to create a definition file descriptor
			in := sif.DescriptorInput{
				Datatype: sif.DataGenericJSON,
				Groupid:  sif.DescrDefaultGroup,
				Link:     sif.DescrUnusedLink,
				Data:     b.JSONObjects[name],
				Fname:    name,
			}
			in.Size = int64(binary.Size(in.Data))

			// add this descriptor input element to creation descriptor slice
			cinfo.InputDescr = append(cinfo.InputDescr, in)
		}
	}

	// data we need to create a system partition descriptor
	parinput := sif.DescriptorInput{
		Datatype: sif.DataPartition,
		Groupid:  sif.DescrDefaultGroup,
		Link:     sif.DescrUnusedLink,
		Fname:    squashfile,
	}
	// open up the data object file for this descriptor
	fp, err := os.Open(parinput.Fname)
	if err != nil {
		return fmt.Errorf("while opening partition file: %s", err)
	}

	defer fp.Close()

	fi, err := fp.Stat()
	if err != nil {
		return fmt.Errorf("while calling stat on partition file: %s", err)
	}

	parinput.Fp = fp
	parinput.Size = fi.Size()

	sifType := sif.FsSquash

	if encOpts != nil {
		sifType = sif.FsEncryptedSquashfs
	}

	err = parinput.SetPartExtra(sifType, sif.PartPrimSys, sif.GetSIFArch(arch))
	if err != nil {
		return
	}

	// add this descriptor input element to the list
	cinfo.InputDescr = append(cinfo.InputDescr, parinput)

	if encOpts != nil {
		data, err := cryptkey.EncryptKey(encOpts.keyInfo, encOpts.plaintext)
		if err != nil {
			return fmt.Errorf("while encrypting filesystem key: %s", err)
		}

		if data != nil {
			syspartID := uint32(len(cinfo.InputDescr))
			part := sif.DescriptorInput{
				Datatype: sif.DataCryptoMessage,
				Groupid:  sif.DescrDefaultGroup,
				Link:     syspartID,
				Data:     data,
				Size:     int64(len(data)),
			}

			// extra data needed for the creation of a signature descriptor
			err := part.SetCryptoMsgExtra(sif.FormatPEM, sif.MessageRSAOAEP)
			if err != nil {
				return err
			}

			cinfo.InputDescr = append(cinfo.InputDescr, part)
		}
	}

	// remove anything that may exist at the build destination at last moment
	os.RemoveAll(path)

	// test container creation with two partition input descriptors
	f, err := sif.CreateContainer(cinfo)
	if err != nil {
		return fmt.Errorf("while creating container: %s", err)
	}

	if err := f.UnloadContainer(); err != nil {
		return fmt.Errorf("while unloading container: %w", err)
	}

	// chown the sif file to the calling user
	if uid, gid, ok := changeOwner(); ok {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("while changing image ownership: %s", err)
		}
	}

	return nil
}

// Assemble creates a SIF image from a Bundle.
func (a *SIFAssembler) Assemble(b *types.Bundle, path string) error {
	sylog.Infof("Creating SIF file...")

	s := packer.NewSquashfs()
	s.MksquashfsPath = a.MksquashfsPath

	f, err := ioutil.TempFile(b.TmpDir, "squashfs-")
	if err != nil {
		return fmt.Errorf("while creating temporary file for squashfs: %v", err)
	}

	fsPath := f.Name()
	f.Close()
	defer os.Remove(fsPath)

	flags := []string{"-noappend"}
	// build squashfs with all-root flag when building as a user
	if syscall.Getuid() != 0 {
		flags = append(flags, "-all-root")
	}
	// specify compression if needed
	if a.GzipFlag {
		flags = append(flags, "-comp", "gzip")
	}
	if a.MksquashfsMem != "" {
		flags = append(flags, "-mem", a.MksquashfsMem)
	}
	if a.MksquashfsProcs != 0 {
		flags = append(flags, "-processors", fmt.Sprint(a.MksquashfsProcs))
	}
	arch := machine.ArchFromContainer(b.RootfsPath)
	if arch == "" {
		sylog.Infof("Architecture not recognized, use native")
		arch = runtime.GOARCH
	}
	sylog.Verbosef("Set SIF container architecture to %s", arch)

	if err := s.Create([]string{b.RootfsPath}, fsPath, flags); err != nil {
		return fmt.Errorf("while creating squashfs: %v", err)
	}

	var encOpts *encryptionOptions

	if b.Opts.EncryptionKeyInfo != nil {
		plaintext, err := cryptkey.NewPlaintextKey(*b.Opts.EncryptionKeyInfo)
		if err != nil {
			return fmt.Errorf("unable to obtain encryption key: %+v", err)
		}

		// A dm-crypt device needs to be created with squashfs
		cryptDev := &crypt.Device{}

		// TODO (schebro): Fix #3876
		// Detach the following code from the squashfs creation. SIF can be
		// created first and encrypted after. This gives the flexibility to
		// encrypt an existing SIF
		loopPath, err := cryptDev.EncryptFilesystem(fsPath, plaintext)
		if err != nil {
			return fmt.Errorf("unable to encrypt filesystem at %s: %+v", fsPath, err)
		}
		defer os.Remove(loopPath)

		fsPath = loopPath

		encOpts = &encryptionOptions{
			keyInfo:   *b.Opts.EncryptionKeyInfo,
			plaintext: plaintext,
		}

	}

	err = createSIF(path, b, fsPath, encOpts, arch)
	if err != nil {
		return fmt.Errorf("while creating SIF: %v", err)
	}

	return nil
}

// changeOwner check the command being called with sudo with the environment
// variable SUDO_COMMAND. Pattern match that for the singularity bin.
func changeOwner() (int, int, bool) {
	r := regexp.MustCompile("(singularity)")
	sudoCmd := os.Getenv("SUDO_COMMAND")
	if !r.MatchString(sudoCmd) {
		return 0, 0, false
	}

	if os.Getenv("SUDO_USER") == "" || syscall.Getuid() != 0 {
		return 0, 0, false
	}

	_uid := os.Getenv("SUDO_UID")
	_gid := os.Getenv("SUDO_GID")
	if _uid == "" || _gid == "" {
		sylog.Warningf("Env vars SUDO_UID or SUDO_GID are not set, won't call chown over built SIF")

		return 0, 0, false
	}

	uid, err := strconv.Atoi(_uid)
	if err != nil {
		sylog.Warningf("Error while calling strconv: %v", err)

		return 0, 0, false
	}
	gid, err := strconv.Atoi(_gid)
	if err != nil {
		sylog.Warningf("Error while calling strconv : %v", err)

		return 0, 0, false
	}

	return uid, gid, true
}
