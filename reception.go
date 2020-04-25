package main

// TODO: Add a command-line switch to handle hearing maps vs. reception maps
// TODO: Add CERT neighborhood abbreviations and legend
// TODO: Add frequency
// TODO: If call sign has dash and it's not found in operator list, try a second time, truncating the dash
// TODO: Don't require a transmitter location to be specifically stated in CSV (call,call,Trans); instead
//       plot transmitter icons from the list of known transmitters (keys to the reports map)
// TODO: Command line switch to only generate maps for certain operators
// TODO: Frequency should be in report file
// TODO: Consider reading reports out of Google Sheets, instead of CSV
// TODO: Break this one file into several (all in package main)
// TODO: Instead of printing names of files as we go, do a progress bar with https://github.com/schollz/progressbar
// TODO: Consider renaming pointer variable as xyzPtr, or some such
// TODO: See if I'm passing large structs/arrays anywhere, replace with pointers
// TODO: Think about objects/methods

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
	"golang.org/x/image/font"
)

// Latitude-Longitude coordinates
type gpsCoord struct {
	lat, long float64
}

// Station data for one operator
type operatorData struct {
	pixel     image.Point // x-y coordinates of a pixel in an image; y increases downward, x increases to the right
	xmitPwr   float64     // Radio transmitter power, in Watts
	antType   string      // Antenna type
	antGain   float64     // Estimated gain of antenna, in dBi
	antHeight float64     // Antenna height, in feet
}

// Configuration parameters, loaded from reception.cfg file
type config struct {
	IconDirectory string // Directory containing icon image files
	IconSize      uint   // icons will be resized to this dimension before plotting
	NoReportIcon  string // Icon to use for missing data

	MapFile         string    // File containing image of base map
	MapNWCorner     []float64 // GPS lat-long coordinates of upper left corner of base map
	MapSECorner     []float64 // GPS lat-long coordinates of lower right corner of base map
	OperatorFile    string    // Name of file containing data on all operators
	ReportFile      string    // Name of file containing operator reception reports
	OutputDirectory string    // Directory we'll write reception maps into

	FontDPI         float64 // Screen resolution in dots per inch
	FontFile        string  // Name of file containing the TTF font we'll use on the map
	FontHinting     string  // "none" or "full" ("none" seems to look better)
	FontSize        float64 // Font size in points
	FontLineSpacing float64 // Spacing between lines of text - NOT USED
}

var cfg config

var gpsToPixel func(gpsCoord) image.Point

func main() {
	flag.Parse() // TODO: This should load command line flags; do something with them!

	if _, err := toml.DecodeFile("reception.cfg", &cfg); err != nil {
		fmt.Println(err)
		return
	}

	// TODO: Read command line parameters (net date, file locations, etc.) here

	// Load the assets we need to construct the maps
	icons := loadIcons(cfg.IconDirectory)
	baseMap := loadBaseMap(cfg.MapFile)
	gpsToPixel = newGpsToPixel(baseMap)
	mapTextImage, ctx := newDrawing(baseMap)

	// Load data
	operators := loadOperators(cfg.OperatorFile)
	reports, receivers := loadReports(cfg.ReportFile)

	// TODO: Refactor the map plotting section into a function?
	// Now create a map for each transmitter
	for transmitter := range reports {
		// TODO: Instead of creating a new outputMapImage on every loop, maybe do the same as we do with
		// mapTextImage--just create one before entering the loop, and draw the baseMap onto it to clear
		// it on every pass through the loop?
		// fmt.Println("     ...Starting transmitter: ", transmitter)
		b := baseMap.Bounds()
		outputMapImage := image.NewRGBA(b)
		draw.Draw(outputMapImage, b, baseMap, image.Point{}, draw.Src)

		// Just added--should be part of every loop?
		draw.Draw(&mapTextImage, mapTextImage.Bounds(), image.Transparent, image.Point{}, draw.Src)

		drawText := newLegendWriter(mapTextImage, ctx)

		for receiver := range receivers {
			// fmt.Println("          ... Starting receiver: ", receiver)
			report, present := reports[transmitter][receiver]

			// Ignore report entries we don't want to plot icons for
			if !present {
				report = cfg.NoReportIcon
			}
			if report == "99" || report == "4" || report == cfg.NoReportIcon {
				continue
			}

			icon := icons[report]
			operator, ok := operators[receiver]
			if !ok {
				fmt.Println(receiver, "is not in operator file; skipping for transmitter", transmitter)
				continue
			}

			offset := image.Point{
				operator.pixel.X - int(icon.Bounds().Max.X/2),
				operator.pixel.Y - int(icon.Bounds().Max.Y/2)}

			draw.Draw(outputMapImage, icon.Bounds().Add(offset), icon, image.Point{}, draw.Over)

			// Add call sign for this receiver
			pt := freetype.Pt(operator.pixel.X+int((icon.Bounds().Max.X+int(cfg.FontSize))/2),
				operator.pixel.Y+int(cfg.FontSize*cfg.FontDPI/72.0/2.0+0.5))
			_, err := ctx.DrawString(receiver, pt)
			if err != nil {
				log.Println(err)
				return
			}
		}

		// Write legend onto image
		// TODO: Using -100 for "no value" to get around Google Sheets exporting empty fields is horrible--do better.

		drawText([]string{"Reception Map for " + transmitter,
			"Frequency: 146.535 MHz Simplex"})

		pwr := operators[transmitter].xmitPwr
		if pwr != -100.0 {
			drawText([]string{fmt.Sprintf("Transmitter Power: %.0f Watts", pwr)})
		}

		ant := operators[transmitter].antType
		if ant != "" {
			drawText([]string{"Antenna Type: " + ant})
		}

		height := operators[transmitter].antHeight
		if height != -100.0 {
			drawText([]string{fmt.Sprintf("Antenna Height: %.0f feet", height)})
		}

		gain := operators[transmitter].antGain
		if gain != -100 {
			drawText([]string{fmt.Sprintf("Antenna Est. Gain: %.1f dBi", gain)})
		}

		draw.Draw(outputMapImage, mapTextImage.Bounds(), &mapTextImage, image.Point{}, draw.Over)

		outputFile := cfg.OutputDirectory + "/" + transmitter + "-xmit-map" + ".png"
		f, err := os.Create(outputFile)
		if err != nil {
			log.Fatalf("failed to create: %s", err)
		}

		png.Encode(f, outputMapImage)
		f.Close()
		fmt.Println("...Map created:", outputFile)
	}

	fmt.Println("All finished!")
}

// Load and resize icons
func loadIcons(dir string) map[string]image.Image {
	fileInfos, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}

	icons := make(map[string]image.Image)

	for _, fileInfo := range fileInfos {
		r, err := os.Open(dir + "/" + fileInfo.Name())
		if err != nil {
			log.Fatal(err)
		}
		defer r.Close()

		icon, err := png.Decode(r)
		if err != nil {
			log.Fatal(err)
		}

		receptionType := strings.TrimSuffix(fileInfo.Name(), ".png")
		icons[receptionType] = resize.Resize(cfg.IconSize, 0, icon, resize.Bilinear)
	}

	return icons
}

// Read the static base map file and return its image data
func loadBaseMap(imageFile string) image.Image {
	f, err := os.Open(imageFile)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	mapImage, err := png.Decode(f)
	if err != nil {
		log.Fatal(err)
	}
	return mapImage

}

// Returns a closure that converts GPS coordinates into an X/Y pixel position on a map image
func newGpsToPixel(mapImage image.Image) func(gpsCoord) image.Point {
	// TODO: Look at passing in map corners as parameters, rather than using globals

	// We use UTM coordinates as an intermediate step between polar GPS goodinates and pixel
	// coordinates; UTM provide a flat, linear mapping of spherical lat/long that is easy
	// to scale to the image pixel coordinates we need.
	//
	// We throw away the zone number and zone letter components when we convert to UTM;
	// they won't matter if the locations are within a few hundred miles of each other
	// TODO: Test that the zone numbers are +/- 1 from each other

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
			log.Fatalln("Can't convert GPS coordinate to UTM", err)
		}

		return image.Point{
			int(((easting - eastingNW) / xMetersPerPixel) + 0.5),
			int(((northingNW - northing) / yMetersPerPixel) + 0.5)}
	}
}

// Load operator data from a specified CSV file and return a map containing operator
// data for each call sign. The CSV has no header row, and each record consists of 7 values:
// call sign, lat, long, transmitter power (W), antenna type, antenna gain (dBi), and antenna height (ft)
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
			log.Fatal(err)
		}

		callsign := strings.ToUpper(record[0])
		lat, err := strconv.ParseFloat(record[1], 64)
		if err != nil {
			log.Fatal(err)
		}
		long, err := strconv.ParseFloat(record[2], 64)
		if err != nil {
			log.Fatal(err)
		}
		xmitPwr, err := strconv.ParseFloat(record[3], 64)
		if err != nil {
			log.Fatal(err)
		}
		antType := record[4]
		antGain, err := strconv.ParseFloat(record[5], 64)
		if err != nil {
			log.Fatal(err)
		}
		antHeight, err := strconv.ParseFloat(record[6], 64)
		if err != nil {
			log.Fatal(err)
		}

		operators[callsign] = operatorData{
			pixel:     gpsToPixel(gpsCoord{lat, long}),
			xmitPwr:   xmitPwr,
			antType:   antType,
			antGain:   antGain,
			antHeight: antHeight}
	}

	return operators
}

// Load operator reception reports. Returns (1) a map of maps; outer key is the transmitter; nested key is
// the receiver; (2) a map whose keys are every receiver in the file, reporting on any transmitter.
func loadReports(csvFile string) (map[string]map[string]string, map[string]bool) {
	f, err := os.Open(csvFile)
	if err != nil {
		log.Fatalln("Couldn't open the report csv file:", err)
	}
	defer f.Close()

	reports := make(map[string]map[string]string)
	receivers := make(map[string]bool)

	r := csv.NewReader(bufio.NewReader(f))

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}

		transmitter := strings.ToUpper(record[0])
		receiver := strings.ToUpper(record[1])
		report := record[2]

		_, present := reports[transmitter]
		if !present {
			reports[transmitter] = make(map[string]string)
		}

		reports[transmitter][receiver] = report
		receivers[receiver] = true
	}

	return reports, receivers
}

// Function returns a blank image for drawing text onto, and a Freetype context for doing that
// drawing that's been initialized with our chosen font.
func newDrawing(baseMap image.Image) (image.RGBA, freetype.Context) {
	// Read and parse the font we'll use
	fontBytes, err := ioutil.ReadFile(cfg.FontFile)
	if err != nil {
		log.Fatalln(err)
	}
	f, err := freetype.ParseFont(fontBytes)
	if err != nil {
		log.Fatalln(err)
	}
	// Initialize the context for plotting text. We plot all text onto a single context,
	// then draw that context onto the main map image after all the icons have been plotted.

	mapTextImage := image.NewRGBA(baseMap.Bounds())
	draw.Draw(mapTextImage, mapTextImage.Bounds(), image.Transparent, image.Point{}, draw.Src)

	ctx := freetype.NewContext()
	ctx.SetDPI(cfg.FontDPI)
	ctx.SetFont(f)
	ctx.SetFontSize(cfg.FontSize)
	ctx.SetClip(mapTextImage.Bounds())
	ctx.SetDst(mapTextImage)
	ctx.SetSrc(&image.Uniform{color.RGBA{0x10, 0x10, 0x10, 0xff}}) // Color of text
	switch cfg.FontHinting {
	default:
		ctx.SetHinting(font.HintingNone)
	case "full":
		ctx.SetHinting(font.HintingFull)
	}

	return *mapTextImage, *ctx
}

// Returns a function closure that takes an array of strings and plots them onto an image, one element per line.
// Cursor location is is wrapped in the closure, so the function can be called repeatedly to plot additional arrays of
// strings onto the image.
func newLegendWriter(textImage image.RGBA, context freetype.Context) func([]string) {

	// TODO: Make margins, line spacing, and positioning configurable
	cursorX := int(cfg.FontSize*5 + 0.5)
	cursorY := textImage.Bounds().Max.Y - int(cfg.FontSize*cfg.FontLineSpacing*cfg.FontDPI/72.0*8+0.5)

	return func(legendItems []string) {
		for _, legend := range legendItems {
			cursor := freetype.Pt(cursorX, cursorY)
			_, err := context.DrawString(legend, cursor)
			if err != nil {
				log.Fatalln(err)
			}
			cursorY += int(cfg.FontSize*cfg.FontLineSpacing*cfg.FontDPI/72.0 + 0.5)
		}

		return
	}
}
