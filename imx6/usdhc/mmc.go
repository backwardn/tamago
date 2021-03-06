// NXP Ultra Secured Digital Host Controller (uSDHC) driver
// https://github.com/f-secure-foundry/tamago
//
// IP: https://www.mobiveil.com/esdhc/
//
// Copyright (c) F-Secure Corporation
// https://foundry.f-secure.com
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package usdhc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/f-secure-foundry/tamago/bits"
)

// MMC registers
const (
	// p181, 7.1 OCR register, JESD84-B51
	MMC_OCR_BUSY        = 31
	MMC_OCR_ACCESS_MODE = 29
	MMC_OCR_VDD_HV_MAX  = 23
	MMC_OCR_VDD_HV_MIN  = 15
	MMC_OCR_VDD_LV      = 7

	ACCESS_MODE_BYTE   = 0b00
	ACCESS_MODE_SECTOR = 0b10

	// p62, 6.6.1 Command sets and extended settings, JESD84-B51
	MMC_SWITCH_ACCESS  = 24
	MMC_SWITCH_INDEX   = 16
	MMC_SWITCH_VALUE   = 8
	MMC_SWITCH_CMD_SET = 0

	ACCESS_WRITE_BYTE = 0b11

	// p184 7.3 CSD register, JESD84-B51
	MMC_CSD_C_SIZE_MULT = 47 + CSD_RSP_OFF
	MMC_CSD_C_SIZE      = 62 + CSD_RSP_OFF
	MMC_CSD_READ_BL_LEN = 80 + CSD_RSP_OFF
	MMC_CSD_TRAN_SPEED  = 96 + CSD_RSP_OFF
	MMC_CSD_SPEC_VERS   = 122 + CSD_RSP_OFF

	// p186 TRAN_SPEED [103:96], JESD84-B51
	TRAN_SPEED_26MHZ = 0x32

	// p193, 7.4 Extended CSD register, JESD84-B51
	EXT_CSD_BUS_WIDTH = 183
	EXT_CSD_HS_TIMING = 185
	EXT_CSD_SEC_COUNT = 212

	// p222, 7.4.65 HS_TIMING [185], JESD84-B51
	HS_TIMING_HS    = 0x1
	HS_TIMING_HS200 = 0x2
)

// MMC constants
const (
	MMC_DETECT_TIMEOUT     = 1 * time.Second
	MMC_DEFAULT_BLOCK_SIZE = 512
)

// p352, 35.4.6 MMC voltage validation flow chart, IMX6FG
func (hw *USDHC) voltageValidationMMC() (mmc bool, hc bool) {
	var arg uint32

	// CMD1 - SEND_OP_COND
	// p57, 6.4.2 Access mode validation, JESD84-B51

	// sector mode supported
	bits.SetN(&arg, MMC_OCR_ACCESS_MODE, 0b11, ACCESS_MODE_SECTOR)
	// set HV range
	bits.SetN(&arg, MMC_OCR_VDD_HV_MIN, 0x1ff, 0x1ff)

	// p46, 6.3.1 Device reset to Pre-idle state, JESD84-B51
	time.Sleep(1 * time.Millisecond)

	start := time.Now()

	for time.Since(start) <= MMC_DETECT_TIMEOUT {
		// CMD1 - SEND_OP_COND - send operating conditions
		if err := hw.cmd(1, READ, arg, RSP_48, false, false, false, 0); err != nil {
			return false, false
		}

		rsp := hw.rsp(0)

		if bits.Get(&rsp, MMC_OCR_BUSY, 1) == 0 {
			continue
		}

		if bits.Get(&rsp, MMC_OCR_ACCESS_MODE, 0b11) == ACCESS_MODE_SECTOR {
			hc = true
		}

		return true, hc
	}

	return false, false
}

func (hw *USDHC) writeCardRegisterMMC(reg uint32, val uint32) (err error) {
	var arg uint32

	// write MMC_SWITCH_VALUE in register pointed in MMC_SWITCH_INDEX
	bits.SetN(&arg, MMC_SWITCH_ACCESS, 0b11, ACCESS_WRITE_BYTE)
	// set MMC_SWITCH_INDEX to desired register
	bits.SetN(&arg, MMC_SWITCH_INDEX, 0xff, reg)
	// set register value
	bits.SetN(&arg, MMC_SWITCH_VALUE, 0xff, val)

	// CMD6 - SWITCH - switch mode of operation
	err = hw.cmd(6, READ, arg, RSP_48, true, true, false, 0)

	if err != nil {
		return
	}

	// We could use EXT_CSD[GENERIC_CMD6_TIME] for a better tran state
	// timeout, we rather choose to apply a generic timeout for now (as
	// most drivers do).
	return hw.waitState(CURRENT_STATE_TRAN, 500*time.Millisecond)
}

// p128, Table 39 — e•MMC internal sizes and related Units / Granularities, JESD84-B51
func (hw *USDHC) detectCapacityMMC(blockSize int, c_size_mult uint32, c_size uint32, read_bl_len uint32) (err error) {
	// density greater than 2GB
	if c_size > 0xff {
		// emulation mode is assumed for densities greater than 256GB
		hw.card.BlockSize = blockSize
		extCSD := make([]byte, blockSize)

		// CMD8 - SEND_EXT_CSD - read extended device data
		if err = hw.transfer(8, READ, 0, 1, uint32(blockSize), extCSD); err != nil {
			return
		}

		hw.card.Blocks = int(binary.LittleEndian.Uint32(extCSD[EXT_CSD_SEC_COUNT:]))
	} else {
		// p188, 7.3.12 C_SIZE [73:62], JESD84-B51
		hw.card.BlockSize = 2 << (read_bl_len - 1)
		hw.card.Blocks = int((c_size + 1) * (2 << (c_size_mult + 2)))
	}

	return
}

// p352, 35.4.7 MMC card initialization flow chart, IMX6FG
// p58, 6.4.4 Device identification process, JESD84-B51
func (hw *USDHC) initMMC() (err error) {
	var arg uint32
	var bus_width uint32

	// CMD2 - ALL_SEND_CID - get unique card identification
	if err = hw.cmd(2, READ, arg, RSP_136, false, true, false, 0); err != nil {
		return
	}

	// Send CMD3 with a chosen RCA, with value greater than 1,
	// p301, A.6.1 Bus initialization , JESD84-B51.
	hw.rca = (uint32(hw.n) + 1) << RCA_ADDR

	// CMD3 - SET_RELATIVE_ADDR - set relative card address (RCA),
	if err = hw.cmd(3, READ, hw.rca, RSP_48, true, true, false, 0); err != nil {
		return
	}

	if state := (hw.rsp(0) >> STATUS_CURRENT_STATE) & 0b1111; state != CURRENT_STATE_IDENT {
		return fmt.Errorf("card not in ident state (%d)", state)
	}

	// CMD9 - SEND_CSD - read device data
	if err = hw.cmd(9, READ, hw.rca, RSP_136, false, true, false, 0); err != nil {
		return
	}

	// block count multiplier
	c_size_mult := hw.rspVal(MMC_CSD_C_SIZE_MULT, 0b111)
	// block count
	c_size := hw.rspVal(MMC_CSD_C_SIZE, 0xfff)
	// block size
	read_bl_len := hw.rspVal(MMC_CSD_READ_BL_LEN, 0xf)
	// operating frequency
	mhz := hw.rspVal(MMC_CSD_TRAN_SPEED, 0xff)
	// e•MMC specification version
	ver := hw.rspVal(MMC_CSD_SPEC_VERS, 0xf)

	if mhz == TRAN_SPEED_26MHZ {
		// clear clock
		hw.setClock(0, 0)
		// set operating frequency
		hw.setClock(DVS_OP, SDCLKFS_OP)
	} else {
		return fmt.Errorf("unexpected TRAN_SPEED %#x", mhz)
	}

	// CMD7 - SELECT/DESELECT CARD - enter transfer state
	if err = hw.cmd(7, READ, hw.rca, RSP_48_CHECK_BUSY, true, true, false, 0); err != nil {
		return
	}

	err = hw.waitState(CURRENT_STATE_TRAN, 1*time.Millisecond)

	if err != nil {
		return
	}

	// p223, 7.4.67 BUS_WIDTH [183], JESD84-B51
	switch hw.width {
	case 4:
		bus_width = 1
	case 8:
		bus_width = 2
	default:
		return errors.New("unsupported MMC bus width")
	}

	err = hw.writeCardRegisterMMC(EXT_CSD_BUS_WIDTH, bus_width)

	if err != nil {
		return
	}

	err = hw.detectCapacityMMC(MMC_DEFAULT_BLOCK_SIZE, c_size_mult, c_size, read_bl_len)

	if err != nil {
		return
	}

	// Enable High Speed DDR (DDR104) mode only on Version 4.1 or above
	// eMMC cards.
	if ver < 4 {
		return
	}

	// p112, Dual Data Rate mode operation, JESD84-B51
	err = hw.writeCardRegisterMMC(EXT_CSD_HS_TIMING, HS_TIMING_HS)

	if err != nil {
		return
	}

	// p223, 7.4.67 BUS_WIDTH [183], JESD84-B51
	switch hw.width {
	case 4:
		bus_width = 5
	case 8:
		bus_width = 6
	}

	err = hw.writeCardRegisterMMC(EXT_CSD_BUS_WIDTH, bus_width)

	if err != nil {
		return
	}

	// clear clock
	hw.setClock(0, 0)
	// set high speed frequency
	hw.setClock(DVS_HS, SDCLKFS_HS_DDR)

	hw.card.DDR = true
	hw.card.HS = true

	return
}
