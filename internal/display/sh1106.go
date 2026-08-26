package display

import "periph.io/x/conn/v3/i2c"

// sh1106PhysicalWidth is the controller's real GRAM width. Nearly every
// SH1106 module exposes a 128px glass centered over this wider GRAM, so
// columns 0-1 and 130-131 are physically present but never drawn to by a
// 128px-wide framebuffer - periph's ssd1306 driver never clears them
// (Opts.W is capped at 128), which is what left stray lit pixels at the
// panel's right edge. clearGRAM() below writes all 132 columns so nothing
// is left over from power-on.
const sh1106PhysicalWidth = 132

// I2C control byte: prefixes a stream of command bytes (Co=0, D/C=0).
const sh1106CtrlCmd = 0x00

// I2C control byte: prefixes a stream of data (GRAM) bytes (Co=0, D/C=1).
const sh1106CtrlData = 0x40

// sh1106Dev drives an SH1106 panel directly in page-addressing mode, the
// only mode real SH1106 silicon implements - unlike periph's ssd1306
// driver, this never sends the SSD1306-only memory-addressing-mode
// commands (0x20/0x21/0x22) that real SH1106 hardware ignores.
type sh1106Dev struct {
	conn   *i2c.Dev
	width  int // visible width in px, must match the framebuffer passed to draw
	height int // visible height in px, must be a multiple of 8
}

func (d *sh1106Dev) command(cmd ...byte) error {
	return d.conn.Tx(append([]byte{sh1106CtrlCmd}, cmd...), nil)
}

func (d *sh1106Dev) data(b []byte) error {
	return d.conn.Tx(append([]byte{sh1106CtrlData}, b...), nil)
}

// colOffset centers the visible width inside the wider physical GRAM.
func (d *sh1106Dev) colOffset() int {
	return (sh1106PhysicalWidth - d.width) / 2
}

// setAddr selects a GRAM page (8px row band) and physical column to write to
// next. physCol is a raw GRAM column (0..131), not adjusted for colOffset -
// callers pass the offset explicitly so clearGRAM can reach column 0.
func (d *sh1106Dev) setAddr(page, physCol int) error {
	lo := byte(physCol & 0x0F)
	hi := byte(0x10 | ((physCol >> 4) & 0x0F))
	return d.command(byte(0xB0|page), lo, hi)
}

// init runs SH1106's power-on command sequence. It mirrors periph's ssd1306
// init for the commands both chips share (segment remap 0xA1 and COM scan
// direction 0xC8, so on-screen orientation is unchanged) but swaps in
// SH1106's charge-pump command (0xAD, 0x8B) in place of SSD1306's (0x8D,
// 0x14) and drops the SSD1306-only addressing-mode commands entirely, since
// page-addressing is the only mode SH1106 has.
func (d *sh1106Dev) init() error {
	pages := d.height / 8
	return d.command(
		0xAE,       // display off
		0xD5, 0x80, // display clock divide ratio/osc freq
		0xA8, byte(pages*8-1), // multiplex ratio
		0xD3, 0x00, // display offset
		0x40,       // display start line = 0
		0xAD, 0x8B, // charge pump enable (SH1106)
		0xA1,       // segment remap
		0xC8,       // COM output scan direction, remapped
		0xDA, 0x12, // COM pins hardware config
		0x81, 0xFF, // contrast
		0xD9, 0xF1, // pre-charge period
		0xDB, 0x40, // VCOMH deselect level
		0xA4, // resume to RAM content display
		0xA6, // normal (non-inverted) display
		0xAF, // display on
	)
}

// clearGRAM blanks every physical column of every page, including the
// margin outside the visible window that draw() never touches - this is
// the fix for the stray-pixel bug, since that margin's power-on GRAM
// content was never being cleared before.
func (d *sh1106Dev) clearGRAM() error {
	blank := make([]byte, sh1106PhysicalWidth)
	pages := d.height / 8
	for page := 0; page < pages; page++ {
		if err := d.setAddr(page, 0); err != nil {
			return err
		}
		if err := d.data(blank); err != nil {
			return err
		}
	}
	return nil
}

// draw writes an image1bit.VerticalLSB-style framebuffer (Pix[band*width+x],
// LSB = topmost pixel) to the panel, centered inside the physical GRAM.
func (d *sh1106Dev) draw(pix []byte) error {
	offset := d.colOffset()
	pages := d.height / 8
	for page := 0; page < pages; page++ {
		if err := d.setAddr(page, offset); err != nil {
			return err
		}
		row := pix[page*d.width : page*d.width+d.width]
		if err := d.data(row); err != nil {
			return err
		}
	}
	return nil
}

func (d *sh1106Dev) halt() error {
	return d.command(0xAE)
}
