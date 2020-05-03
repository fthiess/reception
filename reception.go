// Copyright 2020 Google LLC

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     https://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// BUG: Calls with dashes appear to not be working
// TODO: If call sign has dash and it's not found in operator list, try a second time, truncating the dash
// TODO: If receiver call has a dash, ignore it and anything beyond it (is that right?)
// TODO: Figure out why cfg elements needs to start with capitals (or do they?)

// TODO: Implement CERT neighborhood labels using existing code + fake operators + transparent icon
// TODO: Allow configuration of output file names: always xmit/rcvr --> cfg, plus a command line option to override
// TODO: Write README file

// FUTURE: Consider using concurrency: use goroutines to generate multiple maps at the same time
// FUTURE: Consider reading reports out of Google Sheets, instead of CSV

// Reception is a program that generates maps from ham operator reception reports.
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/golang/freetype"
	"github.com/im7mortal/UTM"
	"github.com/nfnt/resize"
	"github.com/schollz/progressbar"
	"golang.org/x/image/font"
)

// Latitude-Longitude coordinates
type gpsCoord struct {
	lat, long float64
}

// Station data for one operator
type operatorData struct {
	callsign  string      // Operator call sign
	gps       gpsCoord    // GPS coordinates of operator
	pixel     image.Point // x-y pixel coordinates of operator on the map image; y increases downward, x increases to the right
	xmitPwr   float64     // Operator's radio transmitter power, in Watts
	antType   string      // Operator's antenna type
	antGain   float64     // Estimated gain of operator's antenna, in dBi
	antHeight float64     // Height of operator's antenna, in feet
}

// Configuration parameters, loaded from reception.cfg file
type config struct {
	OperatorFile    string // Name of file containing data on all operators
	ReportFile      string // Name of file containing reception reports
	OutputDirectory string // Directory we'll write reception maps into
	CallSigns       string // Comma-separate call signs to create a map of, or "all" for all in report file
	Frequency       string // Frequency the radio reception was tested at
	RcvMapFlag      bool   // False = create transmit maps; true = create receive maps

	IconDirectory string // Directory containing icon image files
	IconSize      uint   // icons will be resized to this dimension before plotting
	TransIcon     string // Icon to use for transmitter

	MapFile     string    // File containing image of base map
	MapNWCorner []float64 // GPS lat-long coordinates of upper left corner of base map
	MapSECorner []float64 // GPS lat-long coordinates of lower right corner of base map

	FontDPI         float64 // Screen resolution in dots per inch
	FontFile        string  // Name of file containing the TTF font we'll use on the map
	FontHinting     string  // "none" or "full" ("none" seems to look better)
	FontSize        float64 // Font size in points
	FontLineSpacing float64 // Spacing between lines of text - NOT USED
}

// Globals for the package
var (
	cfg        config
	gpsToPixel func(gpsCoord) image.Point
	drawLegend func([]string)
)

func main() {
	// Load configuration information. reception.cfg must be in the same directory as the program itself.
	if _, err := toml.DecodeFile("reception.cfg", &cfg); err != nil {
		log.Fatalln("can't open reception.cfg", err)
	}

	// Parse command line options
	flag.StringVar(&cfg.OperatorFile, "operators", cfg.OperatorFile, "Name of file containing operator information")
	flag.StringVar(&cfg.ReportFile, "reports", cfg.ReportFile, "Name of file containing reception reports to be mapped")
	flag.StringVar(&cfg.CallSigns, "calls", cfg.CallSigns, "Call signs for whom to generate maps, or 'all' for all")
	flag.StringVar(&cfg.Frequency, "freq", cfg.Frequency, "Frequency the radio reception was tested at")
	flag.BoolVar(&cfg.RcvMapFlag, "receive", cfg.RcvMapFlag, "Generate receive maps, instead of transmit maps")
	flag.Parse()

	// Load the assets we need to construct the maps
	icons := loadIcons(cfg.IconDirectory)
	baseMap := loadBaseMap(cfg.MapFile)
	gpsToPixel = newGpsToPixel(baseMap)

	// Load operator and report data
	operators := loadOperators(cfg.OperatorFile)
	reports, receivers, transmitters := loadReports(cfg.ReportFile)

	// If the user said they only want a subset of receivers, update the transmitter map to match them
	if cfg.CallSigns != "ALL" {
		newTransmitters := make(map[string]bool)
		calls := strings.Split(strings.ReplaceAll(strings.ToUpper(cfg.CallSigns), " ", ""), ",")
		for _, call := range calls {
			if transmitters[call] {
				newTransmitters[call] = true // We ignore any asked-for call signs there aren't any reports for
			} else {
				fmt.Printf("Skipping %v: no reports\n", call)
			}
		}
		transmitters = newTransmitters
	}

	// Create maps for each transmitter
	fmt.Println("Beginning map generation...")
	bar := progressbar.New(len(transmitters))
	baseBounds := baseMap.Bounds()
	outputMapPtr := image.NewRGBA(baseBounds)
	textMapPtr, textCtxPtr := newDrawing(baseMap) // Separate layer for labels so they're always on top of icons

	for transmitter := range transmitters {
		// Reset the main and text maps to their base images
		draw.Draw(outputMapPtr, baseBounds, baseMap, image.Point{}, draw.Src)
		draw.Draw(textMapPtr, textMapPtr.Bounds(), image.Transparent, image.Point{}, draw.Src)
		drawLegend = newDrawLegend(textMapPtr, textCtxPtr)

		// Add icons and call signs for each receiver
		for receiver := range receivers {
			if transmitter == receiver {
				continue
			}

			report := reports[transmitter][receiver]
			icon, present := icons[report]

			// Ignore if there's no report for this xmit/rcvr pair, or if there's no icon for the report
			if report == "" || !present {
				continue
			}

			plotIcon(outputMapPtr, icon, operators[receiver], textCtxPtr)
		}

		// Plot the transmitter; we do it last so it isn't potentially covered by one of the receivers
		plotIcon(outputMapPtr, icons[cfg.TransIcon], operators[transmitter], textCtxPtr)

		plotLegend(transmitter, operators[transmitter])

		// Merge the text layer onto the main map
		draw.Draw(outputMapPtr, textMapPtr.Bounds(), textMapPtr, image.Point{}, draw.Over)

		// Finish up: save the map into a png file
		var outputFile string
		if cfg.RcvMapFlag {
			outputFile = cfg.OutputDirectory + "/" + transmitter + "-rcvr-map" + ".png"
		} else {
			outputFile = cfg.OutputDirectory + "/" + transmitter + "-xmit-map" + ".png"
		}

		f, err := os.Create(outputFile)
		if err != nil {
			log.Fatalf("Failed to create output file: %s", err)
		}

		png.Encode(f, outputMapPtr)
		f.Close()
		bar.Add(1)
	}

	fmt.Println("\nMap generation completed!")
}

// Function loadIcons loads and resizes icons
func loadIcons(dir string) map[string]image.Image {
	fileInfos, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Fatal("can't read directory", dir, err)
	}

	icons := make(map[string]image.Image)

	for _, fileInfo := range fileInfos {
		r, err := os.Open(dir + "/" + fileInfo.Name())
		if err != nil {
			log.Fatal("can't open "+fileInfo.Name(), err)
		}
		defer r.Close()

		icon, err := png.Decode(r)
		if err != nil {
			log.Fatal("can't decode "+fileInfo.Name(), err)
		}

		iconName := strings.TrimSuffix(fileInfo.Name(), ".png")
		icons[iconName] = resize.Resize(cfg.IconSize, 0, icon, resize.Bilinear)
	}

	return icons
}

// Read the static base map file and return its image data
func loadBaseMap(imageFile string) image.Image {
	f, err := os.Open(imageFile)
	if err != nil {
		log.Fatal("can't open", imageFile, err)
	}
	defer f.Close()

	mapImage, err := png.Decode(f)
	if err != nil {
		log.Fatal("can't decode base map", imageFile, err)
	}
	return mapImage

}

// Function loadOperators loads operator data from a CSV file and returns a map structure
// containing operator data for each call sign. Each record of the file contains 7 values:
//   - Call sign
//   - Lattitude
//   - Longitude
//   - Transmitter power (W)
//   - Antenna type
//   - Antenna gain (dBi)
//   - Antenna height (ft)
func loadOperators(csvFile string) map[string]operatorData {
	f, err := os.Open(csvFile)
	if err != nil {
		log.Fatalln("Couldn't open the operator csv file:", err)
	}
	defer f.Close()

	operators := make(map[string]operatorData)

	r := csv.NewReader(bufio.NewReader(f))

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal("error reading operator file", csvFile, err)
		}

		callsign := strings.ReplaceAll(strings.ToUpper(record[0]), " ", "")

		lat, err := strconv.ParseFloat(record[1], 64)
		if err != nil {
			log.Fatalln("can't parse latitude in operator CSV", err)
		}
		long, err := strconv.ParseFloat(record[2], 64)
		if err != nil {
			log.Fatalln("can't parse longitude in operator CSV", err)
		}
		gps := gpsCoord{lat, long}

		xmitPwr, err := strconv.ParseFloat(record[3], 64)
		if err != nil {
			log.Fatalln("can't parse transmitter power in operator CSV", err)
		}

		antType := record[4]

		antGain, err := strconv.ParseFloat(record[5], 64)
		if err != nil {
			log.Fatalln("can't parse antenna gain in operator CSV", err)
		}

		antHeight, err := strconv.ParseFloat(record[6], 64)
		if err != nil {
			log.Fatalln("can't parse antenna height in operator CSV", err)
		}

		operators[callsign] = operatorData{
			callsign:  callsign,
			gps:       gps,
			pixel:     gpsToPixel(gps),
			xmitPwr:   xmitPwr,
			antType:   antType,
			antGain:   antGain,
			antHeight: antHeight}
	}

	return operators
}

// FunctionloadReports loads reception reports from a CSV. Each record of the file contains 3 items:
//   - Transmitter call sign
//   - Receiver call sign
//   - Icon name (which is generally the same as the reception quality level)
// The function returns
//   (1) A map of maps whose outer key is the transmitter, and whose nested key is the receiver, and whose
//       values are the icon to use for the transmitter/receiver pair (usually the reception quality level)
//   (2) A map whose keys are every receiver in the file
//   (3) A map whose keys are every transmitter in the file.
// Normally these reports are for tranmission maps, showing reception quality for all receivers that hear one
// transmitter. However, if cfg.RcvMapFlag is true, the user asked for a reception map instead--reception quality
// the transmitter had for all receivers. If we're doing a receive map, we just swap transmitters and receivers as
// we load the reception reports.
func loadReports(csvFile string) (map[string]map[string]string, map[string]bool, map[string]bool) {
	f, err := os.Open(csvFile)
	if err != nil {
		log.Fatalln("couldn't open the report csv file:", err)
	}
	defer f.Close()

	reports := make(map[string]map[string]string)
	receivers := make(map[string]bool)
	transmitters := make(map[string]bool)

	r := csv.NewReader(bufio.NewReader(f))

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}

		var transmitter, receiver string
		if cfg.RcvMapFlag {
			transmitter = strings.ToUpper(record[0])
			receiver = strings.ToUpper(record[1])
		} else {
			transmitter = strings.ToUpper(record[1])
			receiver = strings.ToUpper(record[0])
		}
		report := record[2]

		if reports[transmitter] == nil {
			reports[transmitter] = make(map[string]string)
		}

		reports[transmitter][receiver] = report
		receivers[receiver] = true
		transmitters[transmitter] = true
	}

	return reports, receivers, transmitters
}

// Function plotLegend plots the legend onto the map image
func plotLegend(transmitter string, opData operatorData) {
	// TODO: Using -100 for "no value" to get around Google Sheets exporting empty fields is horrible--do better
	if cfg.RcvMapFlag {
		drawLegend([]string{"Receive Map (who can I hear) for " + transmitter})
	} else {
		drawLegend([]string{"Transmission Map (who can hear me) for " + transmitter})
	}

	drawLegend([]string{"Frequency: " + cfg.Frequency})

	pwr := opData.xmitPwr
	if pwr != -100.0 {
		drawLegend([]string{fmt.Sprintf("Transmitter Power: %.0f Watts", pwr)})
	}

	ant := opData.antType
	if ant != "" {
		drawLegend([]string{"Antenna Type: " + ant})
	}

	height := opData.antHeight
	if height != -100.0 {
		drawLegend([]string{fmt.Sprintf("Antenna Height: %.0f feet", height)})
	}

	gain := opData.antGain
	if gain != -100 {
		drawLegend([]string{fmt.Sprintf("Antenna Est. Gain: %.1f dBi", gain)})
	}

	return
}

// Function newGpsToPixel returns a function closure that converts GPS coordinates into an X/Y pixel position on a map image
func newGpsToPixel(mapImage image.Image) func(gpsCoord) image.Point {
	// We use UTM coordinates as an intermediate step between polar GPS goodinates and pixel
	// coordinates; UTM provide a flat, linear mapping of spherical lat/long that is easy
	// to scale to the image pixel coordinates we need.
	//
	// We throw away the zone number and zone letter components when we convert to UTM;
	// they won't matter if the locations are within a few hundred miles of each other
	// TODO: Test that the zone numbers are +/- 1 from each other, just in case someone does something crazy

	eastingNW, northingNW, _, _, err := UTM.FromLatLon(cfg.MapNWCorner[0], cfg.MapNWCorner[1], false)
	if err != nil {
		log.Fatalln("MapNWCorner can't be converted to UTM", err)
	}
	eastingSE, northingSE, _, _, err := UTM.FromLatLon(cfg.MapSECorner[0], cfg.MapSECorner[1], false)
	if err != nil {
		log.Fatalln("MapSECorner can't be converted to UTM", err)
	}
	xMetersPerPixel := (eastingSE - eastingNW) / float64(mapImage.Bounds().Dx())
	yMetersPerPixel := (northingNW - northingSE) / float64(mapImage.Bounds().Dy())

	return func(gps gpsCoord) image.Point {
		easting, northing, _, _, err := UTM.FromLatLon(gps.lat, gps.long, false)
		if err != nil {
			log.Fatalln("can't convert GPS coordinate to UTM", err)
		}

		return image.Point{
			int(((easting - eastingNW) / xMetersPerPixel) + 0.5),
			int(((northingNW - northing) / yMetersPerPixel) + 0.5)}
	}
}

// Function newDrawing returns a blank image for drawing text onto, and a Freetype context for doing the
// drawing that's been initialized with our chosen font info.
func newDrawing(baseMap image.Image) (*image.RGBA, *freetype.Context) {
	// Read and parse the font we'll use
	fontBytes, err := ioutil.ReadFile(cfg.FontFile)
	if err != nil {
		log.Fatalln("can't open font file", cfg.FontFile, err)
	}
	f, err := freetype.ParseFont(fontBytes)
	if err != nil {
		log.Fatalln("can't parse font file", cfg.FontFile, err)
	}

	// Initialize a blank image for plotting text (icon labels and the legend) onto. After we're done plotting
	// everything for one reception map, we overlay the text image onto the main map image.
	textMapPtr := image.NewRGBA(baseMap.Bounds())
	draw.Draw(textMapPtr, textMapPtr.Bounds(), image.Transparent, image.Point{}, draw.Src)

	ctxPtr := freetype.NewContext()
	ctxPtr.SetDPI(cfg.FontDPI)
	ctxPtr.SetFont(f)
	ctxPtr.SetFontSize(cfg.FontSize)
	ctxPtr.SetClip(textMapPtr.Bounds())
	ctxPtr.SetDst(textMapPtr)
	ctxPtr.SetSrc(&image.Uniform{color.RGBA{0x10, 0x10, 0x10, 0xff}}) // Color of text
	switch cfg.FontHinting {
	default:
		ctxPtr.SetHinting(font.HintingNone)
	case "full":
		ctxPtr.SetHinting(font.HintingFull)
	}
	return textMapPtr, ctxPtr
}

// Function newDrawLegends returns a function closure that takes an slice of strings and plots them onto an image,
// one element per line. Cursor location is is wrapped in the closure, so the function can be called repeatedly
// to plot additional slices of strings onto the image.
func newDrawLegend(textImagePtr *image.RGBA, contextPtr *freetype.Context) func([]string) {

	// TODO: Make margins, line spacing, and positioning configurable
	cursorX := int(cfg.FontSize*5 + 0.5)
	cursorY := textImagePtr.Bounds().Max.Y - int(cfg.FontSize*cfg.FontLineSpacing*cfg.FontDPI/72.0*8+0.5)

	return func(legendItems []string) {
		for _, legend := range legendItems {
			cursor := freetype.Pt(cursorX, cursorY)
			_, err := contextPtr.DrawString(legend, cursor)
			if err != nil {
				log.Fatalln("Can't plot legend string", err)
			}
			cursorY += int(cfg.FontSize*cfg.FontLineSpacing*cfg.FontDPI/72.0 + 0.5)
		}

		return
	}
}

// Function plotIcons plots an icon on the map image
func plotIcon(mapPtr *image.RGBA, icon image.Image, operator operatorData, contextPtr *freetype.Context) {
	if operator.callsign == "" {
		fmt.Println("Skipping icon for missing operator")
		return
	}

	offset := image.Point{
		operator.pixel.X - int(icon.Bounds().Max.X/2),
		operator.pixel.Y - int(icon.Bounds().Max.Y/2)}

	draw.Draw(mapPtr, icon.Bounds().Add(offset), icon, image.Point{}, draw.Over)

	pt := freetype.Pt(operator.pixel.X+int((icon.Bounds().Max.X+int(cfg.FontSize))/2),
		operator.pixel.Y+int(cfg.FontSize*cfg.FontDPI/72.0/2.0+0.5))
	_, err := contextPtr.DrawString(operator.callsign, pt)
	if err != nil {
		log.Fatalln("can't plot icon label", err)
		return
	}
}
