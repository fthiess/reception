package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/im7mortal/UTM"
	"github.com/nfnt/resize"
)

type gpsCoord struct {
	lat, long float64
}

type config struct {
	IconDirectory   string    // Directory containing icon image files
	IconSize        uint      // icons will be resized to this dimension before plotting
	NoReportIcon    string    // Icon to use for missing data
	MapFile         string    // File containing image of base map
	MapNWCorner     []float64 // GPS coordinates of upper left corner of base map
	MapSECorner     []float64 // GPS coordinates of lower right corner of base map
	OperatorFile    string    // GPS coordinates of each operator
	ReportFile      string    // Operator reception reports
	OutputDirectory string    // Directory we'll write reception maps into
}

var cfg config

var gpsToPixel func(gpsCoord) image.Point

func main() {
	if _, err := toml.DecodeFile("reception.cfg", &cfg); err != nil {
		fmt.Println(err)
		return
	}

	// TODO: Read command line parameters (net date, file locations, etc.) here

	// Load the assets we need to construct the maps
	icons := loadIcons(cfg.IconDirectory)
	baseMap := loadBaseMap(cfg.MapFile)
	gpsToPixel = newGpsToPixel(baseMap)

	// Load data
	locations := loadLocations(cfg.OperatorFile)
	reports, receivers := loadReports(cfg.ReportFile)

	// TODO: Refactor the map plotting section into a function?
	// Now create a map for each transmitter
	for transmitter := range reports {
		b := baseMap.Bounds()
		outputMap := image.NewRGBA(b)
		draw.Draw(outputMap, b, baseMap, image.Point{}, draw.Src)

		for receiver := range receivers {
			report, present := reports[transmitter][receiver]
			if !present {
				report = cfg.NoReportIcon
			}

			icon := icons[report]
			location := locations[receiver]
			// TODO: Check if a call sign isn't in the locations.csv file
			// location, present := locations[receiver]; present == nil {
			// 	fmt.Println("Receiver", receiver, "is not in location file; skipping.")
			// 	continue
			}
			offset := image.Point{
				location.X - int(icon.Bounds().Max.X/2),
				location.Y - int(icon.Bounds().Max.Y/2)}

			draw.Draw(outputMap, icon.Bounds().Add(offset), icon, image.Point{}, draw.Over)

			// TODO: Add code here to write call sign onto map
			// TODO: Find a better font
		}

		// TODO: Add code here to add summary information to corner of map

		outputFile := cfg.OutputDirectory + "/" + "reception-map-" + transmitter + ".png"
		f, err := os.Create(outputFile)
		if err != nil {
			log.Fatalf("failed to create: %s", err)
		}

		png.Encode(f, outputMap)
		f.Close()
		fmt.Println("...Map created:", outputFile)
	}
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

// Load operator lat/long locations from a specified CSV file and return a map containing
// image coordinates for each call sign. The CSV has no header row, and each record
// consists of 3 values: call sign, lattitude, longitude.
func loadLocations(csvFile string) map[string]image.Point {
	f, err := os.Open(csvFile)
	if err != nil {
		log.Fatalln("Couldn't open the operator csv file:", err)
	}
	defer f.Close()

	locations := make(map[string]image.Point)

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

		locations[callsign] = gpsToPixel(gpsCoord{lat, long})
	}

	return locations
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
