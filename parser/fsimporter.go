package parser

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/activecm/rita/config"
	"github.com/activecm/rita/database"
	"github.com/activecm/rita/parser/conn"
	"github.com/activecm/rita/parser/explodeddns"
	fpt "github.com/activecm/rita/parser/fileparsetypes"
	"github.com/activecm/rita/parser/freq"
	"github.com/activecm/rita/parser/host"
	"github.com/activecm/rita/parser/hostname"
	"github.com/activecm/rita/parser/parsetypes"
	"github.com/activecm/rita/parser/uconn"
	"github.com/activecm/rita/resources"
	"github.com/activecm/rita/util"
	log "github.com/sirupsen/logrus"
)

type (
	//FSImporter provides the ability to import bro files from the file system
	FSImporter struct {
		res             *resources.Resources
		indexingThreads int
		parseThreads    int
		internal        []*net.IPNet
		alwaysIncluded  []*net.IPNet
		neverIncluded   []*net.IPNet
		connLimit       int64
	}
)

//NewFSImporter creates a new file system importer
func NewFSImporter(res *resources.Resources,
	indexingThreads int, parseThreads int) *FSImporter {
	return &FSImporter{
		res:             res,
		indexingThreads: indexingThreads,
		parseThreads:    parseThreads,
		internal:        getParsedSubnets(res.Config.S.Filtering.InternalSubnets),
		alwaysIncluded:  getParsedSubnets(res.Config.S.Filtering.AlwaysInclude),
		neverIncluded:   getParsedSubnets(res.Config.S.Filtering.NeverInclude),
		connLimit:       int64(res.Config.S.Strobe.ConnectionLimit),
	}
}

//Run starts importing a given path into a datastore
func (fs *FSImporter) Run(datastore Datastore) {
	// track the time spent parsing
	start := time.Now()
	fs.res.Log.WithFields(
		log.Fields{
			"start_time": start.Format(util.TimeFormat),
		},
	).Info("Starting filesystem import. Collecting file details.")

	fmt.Println("\t[-] Finding files to parse")
	//find all of the bro log paths
	files := readDir(fs.res.Config.S.Bro.ImportDirectory, fs.res.Log)

	//hash the files and get their stats
	indexedFiles := indexFiles(files, fs.indexingThreads, fs.res.Config, fs.res.Log)

	progTime := time.Now()
	fs.res.Log.WithFields(
		log.Fields{
			"current_time": progTime.Format(util.TimeFormat),
			"total_time":   progTime.Sub(start).String(),
		},
	).Info("Finished collecting file details. Starting upload.")

	indexedFiles = removeOldFilesFromIndex(indexedFiles, fs.res.MetaDB, fs.res.Log)

	filterHugeUconnsMap, uconnMap, explodeddnsMap, hostnameMap := fs.parseFiles(indexedFiles, fs.parseThreads, datastore, fs.res.Log)

	// build or update the exploded DNS table
	fs.buildExplodedDNS(explodeddnsMap)

	// build or update the exploded DNS table
	fs.buildHostnames(hostnameMap)

	// Must wait for all inserts to finish before attempting to delete
	datastore.Flush()
	fs.bulkRemoveHugeUconns(indexedFiles[0].TargetDatabase, filterHugeUconnsMap, uconnMap)
	fs.buildUconns(uconnMap)
	fs.buildHosts(uconnMap)

	fmt.Println("\t[-] Waiting for all inserts to finish... ")

	fmt.Println("\t[-] Indexing log entries")
	updateFilesIndex(indexedFiles, fs.res.MetaDB, fs.res.Log)

	progTime = time.Now()
	fs.res.Log.WithFields(
		log.Fields{
			"current_time": progTime.Format(util.TimeFormat),
			"total_time":   progTime.Sub(start).String(),
		},
	).Info("Finished upload. Starting indexing")

	datastore.Index()

	progTime = time.Now()
	fs.res.Log.WithFields(
		log.Fields{
			"current_time": progTime.Format(util.TimeFormat),
			"total_time":   progTime.Sub(start).String(),
		},
	).Info("Finished importing log files")

	fmt.Println("\t[-] Done!")
}

// readDir recursively reads the directory looking for log and .gz files
func readDir(cpath string, logger *log.Logger) []string {
	var toReturn []string
	files, err := ioutil.ReadDir(cpath)
	if err != nil {
		logger.WithFields(log.Fields{
			"error": err.Error(),
			"path":  cpath,
		}).Error("Error when reading directory")
	}

	for _, file := range files {
		// Stop RITA from following symlinks
		// In the case that RITA is pointed directly at Bro, it should not
		// parse the "current" symlink which points to the spool.
		if file.IsDir() && file.Mode() != os.ModeSymlink {
			toReturn = append(toReturn, readDir(path.Join(cpath, file.Name()), logger)...)
		}
		if strings.HasSuffix(file.Name(), "gz") ||
			strings.HasSuffix(file.Name(), "log") {
			toReturn = append(toReturn, path.Join(cpath, file.Name()))
		}
	}
	return toReturn
}

//indexFiles takes in a list of bro files, a number of threads, and parses
//some metadata out of the files
func indexFiles(files []string, indexingThreads int,
	cfg *config.Config, logger *log.Logger) []*fpt.IndexedFile {
	n := len(files)
	output := make([]*fpt.IndexedFile, n)
	indexingWG := new(sync.WaitGroup)

	for i := 0; i < indexingThreads; i++ {
		indexingWG.Add(1)

		go func(files []string, indexedFiles []*fpt.IndexedFile,
			sysConf *config.Config, logger *log.Logger,
			wg *sync.WaitGroup, start int, jump int, length int) {

			for j := start; j < length; j += jump {
				indexedFile, err := newIndexedFile(files[j], cfg, logger)
				if err != nil {
					logger.WithFields(log.Fields{
						"file":  files[j],
						"error": err.Error(),
					}).Warning("An error was encountered while indexing a file")
					//errored on files will be nil
					continue
				}
				indexedFiles[j] = indexedFile
			}
			wg.Done()
		}(files, output, cfg, logger, indexingWG, i, indexingThreads, n)
	}

	indexingWG.Wait()
	return output
}

//parseFiles takes in a list of indexed bro files, the number of
//threads to use to parse the files, whether or not to sort data by date,
//a MongoDB datastore object to store the bro data in, and a logger to report
//errors and parses the bro files line by line into the database.
func (fs *FSImporter) parseFiles(indexedFiles []*fpt.IndexedFile, parsingThreads int, datastore Datastore, logger *log.Logger) (
	[]uconn.Pair, map[string]uconn.Pair, map[string]int, map[string][]string) {
	// Counts the number of uconns per source-destination pair
	explodeddnsMap := make(map[string]int)

	hostnameMap := make(map[string][]string)

	//set up parallel parsing
	n := len(indexedFiles)
	parsingWG := new(sync.WaitGroup)

	// Counts the number of uconns per source-destination pair
	uconnMap := make(map[string]uconn.Pair)

	// map to hold the too many connections uconns
	var filterHugeUconnsMap []uconn.Pair

	// Creates a mutex for locking map keys during read-write operations
	var mutex = &sync.Mutex{}

	for i := 0; i < parsingThreads; i++ {
		parsingWG.Add(1)

		go func(indexedFiles []*fpt.IndexedFile, logger *log.Logger,
			wg *sync.WaitGroup, start int, jump int, length int) {
			//comb over array
			for j := start; j < length; j += jump {
				fmt.Println("\t[-] Parsing " + indexedFiles[j].Path + " -> " + indexedFiles[j].TargetDatabase)
				//read the file
				fileHandle, err := os.Open(indexedFiles[j].Path)
				if err != nil {
					logger.WithFields(log.Fields{
						"file":  indexedFiles[j].Path,
						"error": err.Error(),
					}).Error("Could not open file for parsing")
				}
				fileScanner, err := getFileScanner(fileHandle)
				if err != nil {
					logger.WithFields(log.Fields{
						"file":  indexedFiles[j].Path,
						"error": err.Error(),
					}).Error("Could not open file for parsing")
				}

				for fileScanner.Scan() {
					if fileScanner.Err() != nil {
						break
					}

					//parse the line
					data := parseLine(
						fileScanner.Text(),
						indexedFiles[j].GetHeader(),
						indexedFiles[j].GetFieldMap(),
						indexedFiles[j].GetBroDataFactory(),
						logger,
					)

					// The number of conns in a uconn
					// var connCount int64

					if data != nil {
						//figure out which collection (dns, http, or conn) this line is heading for
						targetCollection := indexedFiles[j].TargetCollection
						targetDB := indexedFiles[j].TargetDatabase

						/// *************************************************************///
						///                           CONNS                              ///
						/// *************************************************************///
						if targetCollection == fs.res.Config.T.Structure.ConnTable {

							// Use reflection to access the conn entry's fields. At this point inside
							// the if statement we know parseConn is a "conn" instance, but the code
							// assumes a generic "BroType" interface.
							parseConn := reflect.ValueOf(data).Elem()

							// get source destination pair for connection record
							uconnPair := uconn.Pair{
								Src: parseConn.FieldByName("Source").Interface().(string),
								Dst: parseConn.FieldByName("Destination").Interface().(string),
							}

							// Run conn pair through filter to filter out certain connections
							ignore := false //fs.filterConnPair(uconn.src, uconn.dst)

							// If connection pair is not subject to filtering, process
							if !ignore {

								// NOTE : isLocal needs to be updated via filtering instead !!!
								uconnPair.IsLocalSrc = parseConn.FieldByName("LocalOrigin").Interface().(bool)
								uconnPair.IsLocalDst = parseConn.FieldByName("LocalResponse").Interface().(bool)
								ts := parseConn.FieldByName("TimeStamp").Interface().(int64)
								origIPBytes := parseConn.FieldByName("OrigIPBytes").Interface().(int64)
								respIPBytes := parseConn.FieldByName("RespIPBytes").Interface().(int64)
								duration := float64(parseConn.FieldByName("Duration").Interface().(float64))
								bytes := int64(origIPBytes + respIPBytes)

								// Concatenate the source and destination IPs to use as a map key
								srcDst := uconnPair.Src + uconnPair.Dst

								// Safely store the number of conns for this uconn
								mutex.Lock()

								// Increment the connection count for the src-dst pair
								connCount := uconnMap[srcDst].ConnectionCount + 1
								uconnPair.ConnectionCount = connCount

								// Only append unique timestamps to tslist
								timestamps := uconnMap[srcDst].TsList
								if isUniqueTimestamp(ts, timestamps) {
									uconnPair.TsList = append(timestamps, ts)
								} else {
									uconnPair.TsList = timestamps
								}

								// Append all origIPBytes to origBytesList
								uconnPair.OrigBytesList = append(uconnMap[srcDst].OrigBytesList, origIPBytes)

								// Calculate and store the total number of bytes exchanged by the uconn pair
								uconnPair.TotalBytes = uconnMap[srcDst].TotalBytes + bytes

								// Calculate and store the average number of bytes
								uconnPair.AvgBytes = float64(((int64(uconnMap[srcDst].AvgBytes) * connCount) + bytes) / (connCount + 1))

								// Calculate and store the total duration
								uconnPair.TotalDuration = uconnMap[srcDst].TotalDuration + duration

								// Replace existing duration if current duration is higher
								if duration > uconnMap[srcDst].MaxDuration {
									uconnPair.MaxDuration = duration
								} else {
									uconnPair.MaxDuration = uconnMap[srcDst].MaxDuration
								}
								uconnMap[srcDst] = uconn.Pair{
									Src:             uconnPair.Src,
									Dst:             uconnPair.Dst,
									ConnectionCount: uconnPair.ConnectionCount,
									IsLocalSrc:      uconnPair.IsLocalSrc,
									IsLocalDst:      uconnPair.IsLocalDst,
									TotalBytes:      uconnPair.TotalBytes,
									AvgBytes:        uconnPair.AvgBytes,
									TotalDuration:   uconnPair.TotalDuration,
									MaxDuration:     uconnPair.MaxDuration,
									TsList:          uconnPair.TsList,
									OrigBytesList:   uconnPair.OrigBytesList,
								}

								// stores the conn record in conn collection if below threshold
								if connCount < fs.connLimit {
									datastore.Store(&ImportedData{
										BroData:          data,
										TargetDatabase:   targetDB,
										TargetCollection: targetCollection,
									})
								} else if connCount == fs.connLimit {
									// tag strobe for removal from conns after import
									filterHugeUconnsMap = append(filterHugeUconnsMap, uconnPair)
								}

								mutex.Unlock()
							}

							/// *************************************************************///
							///                             DNS                             ///
							/// *************************************************************///
						} else if targetCollection == fs.res.Config.T.Structure.DNSTable {
							parseDNS := reflect.ValueOf(data).Elem()

							domain := parseDNS.FieldByName("Query").Interface().(string)
							queryTypeName := parseDNS.FieldByName("QTypeName").Interface().(string)

							// Safely store the number of conns for this uconn
							mutex.Lock()

							// increment domain map count for exploded dns
							explodeddnsMap[domain]++

							// Increment the connection count for the src-dst pair
							if _, ok := hostnameMap[domain]; !ok {
								hostnameMap[domain] = []string{}
							}

							if queryTypeName == "A" {
								answers := parseDNS.FieldByName("Answers").Interface().([]string)
								for _, answer := range answers {
									// Check if answer is an IP address
									if net.ParseIP(answer) != nil {
										hostnameMap[domain] = append(hostnameMap[domain], answer)
										// hostname.IPs = append(hostname.IPs, answer)
									}
								}
							}

							mutex.Unlock()

						} else {
							// We do not limit any of the other log types
							datastore.Store(&ImportedData{
								BroData:          data,
								TargetDatabase:   targetDB,
								TargetCollection: targetCollection,
							})
						}

					}
				}
				indexedFiles[j].ParseTime = time.Now()
				fileHandle.Close()
				logger.WithFields(log.Fields{
					"path": indexedFiles[j].Path,
				}).Info("Finished parsing file")
			}
			wg.Done()
		}(indexedFiles, logger, parsingWG, i, parsingThreads, n)
	}
	parsingWG.Wait()

	return filterHugeUconnsMap, uconnMap, explodeddnsMap, hostnameMap
}

func isUniqueTimestamp(timestamp int64, timestamps []int64) bool {
	for _, val := range timestamps {
		if val == timestamp {
			return false
		}
	}
	return true
}

//buildExplodedDNS .....
func (fs *FSImporter) buildExplodedDNS(domainMap map[string]int) {
	fmt.Println("\t[-] Creating Exploded DNS Collection")
	// Set up the database
	explodedDNSRepo := explodeddns.NewMongoRepository(fs.res)
	explodedDNSRepo.CreateIndexes()
	explodedDNSRepo.Upsert(domainMap)
}

//buildHostnames .....
func (fs *FSImporter) buildHostnames(hostnameMap map[string][]string) {
	fmt.Println("\t[-] Creating Hostnames Collection")
	// Set up the database
	hostnameRepo := hostname.NewMongoRepository(fs.res)
	hostnameRepo.CreateIndexes()
	hostnameRepo.Upsert(hostnameMap)
}

func (fs *FSImporter) buildUconns(uconnMap map[string]uconn.Pair) {
	fmt.Println("\t[-] Creating Uconns Collection")

	uconnRepo := uconn.NewMongoRepository(fs.res)

	err := uconnRepo.CreateIndexes()
	if err != nil {
		fs.res.Log.Error(err)
	}

	// add uconn pair to uconn table
	uconnRepo.Upsert(uconnMap)

}

func (fs *FSImporter) buildHosts(uconnMap map[string]uconn.Pair) {
	fmt.Println("\t[-] Creating Hosts Collection")
	hostRepo := host.NewMongoRepository(fs.res)

	err := hostRepo.CreateIndexes()
	if err != nil {
		fs.res.Log.Error(err)
	}

	for entry := range uconnMap {

		// **** add uconn src to hosts table if it doesn't already exist *** //
		if isIPv4(uconnMap[entry].Src) {
			host := &parsetypes.Host{
				IP:          uconnMap[entry].Src,
				Local:       uconnMap[entry].IsLocalSrc,
				IPv4:        isIPv4(uconnMap[entry].Src),
				MaxDuration: float32(uconnMap[entry].MaxDuration),
				IPv4Binary:  ipv4ToBinary(net.ParseIP(uconnMap[entry].Src)),
			}
			// update hosts field
			hostRepo.Upsert(host, true)
		}

		// **** add uconn dst to hosts table if it doesn't already exist *** //
		if isIPv4(uconnMap[entry].Dst) {
			host := &parsetypes.Host{
				IP:          uconnMap[entry].Dst,
				Local:       uconnMap[entry].IsLocalDst,
				IPv4:        isIPv4(uconnMap[entry].Dst),
				MaxDuration: float32(uconnMap[entry].MaxDuration),
				IPv4Binary:  ipv4ToBinary(net.ParseIP(uconnMap[entry].Dst)),
			}
			// update hosts field
			hostRepo.Upsert(host, false)
		}
	}

}

// bulkRemoveHugeUconns loops through every IP pair in filterHugeUconnsMap and deletes all corresponding
// entries in the "conn" collection. It also creates new entries in the FrequentConnTable collection.
func (fs *FSImporter) bulkRemoveHugeUconns(targetDB string, filterHugeUconnsMap []uconn.Pair, uconnMap map[string]uconn.Pair) {

	connRepo := conn.NewMongoRepository(fs.res)
	freqRepo := freq.NewMongoRepository(fs.res)

	fmt.Println("\t[-] Creating Strobes and removing unused connection info")
	freqConns := make([]*parsetypes.Conn, 0)
	for _, freqConn := range filterHugeUconnsMap {
		freqConns = append(freqConns, &parsetypes.Conn{
			Source:      freqConn.Src,
			Destination: freqConn.Dst,
		})
		freqRepo.Insert(
			&parsetypes.Freq{
				Source:          freqConn.Src,
				Destination:     freqConn.Dst,
				ConnectionCount: freqConn.ConnectionCount,
			})
		// remove entry out of uconns map so it doesn't end up in uconns collection
		srcDst := freqConn.Src + freqConn.Dst
		delete(uconnMap, srcDst)
	}

	// Execute the bulk deletion
	connRepo.BulkDelete(freqConns)
}

//removeOldFilesFromIndex checks all indexedFiles passed in to ensure
//that they have not previously been imported into the same database.
//The files are compared based on their hashes (md5 of first 15000 bytes)
//and the database they are slated to be imported into.
func removeOldFilesFromIndex(indexedFiles []*fpt.IndexedFile,
	metaDatabase *database.MetaDB, logger *log.Logger) []*fpt.IndexedFile {
	var toReturn []*fpt.IndexedFile
	oldFiles, err := metaDatabase.GetFiles()
	if err != nil {
		logger.WithFields(log.Fields{
			"error": err.Error(),
		}).Error("Could not obtain a list of previously parsed files")
	}
	//NOTE: This can be improved to n log n if we need to
	for _, newFile := range indexedFiles {
		if newFile == nil {
			//this file was errored on earlier, i.e. we didn't find a tgtDB etc.
			continue
		}

		have := false
		for _, oldFile := range oldFiles {
			if oldFile.Hash == newFile.Hash && oldFile.TargetDatabase == newFile.TargetDatabase {
				logger.WithFields(log.Fields{
					"path":            newFile.Path,
					"target_database": newFile.TargetDatabase,
				}).Warning("Refusing to import file into the same database twice")
				have = true
				break
			}
		}

		if !have {
			toReturn = append(toReturn, newFile)
		}
	}
	return toReturn
}

//updateFilesIndex updates the files collection in the metaDB with the newly parsed files
func updateFilesIndex(indexedFiles []*fpt.IndexedFile, metaDatabase *database.MetaDB,
	logger *log.Logger) {
	err := metaDatabase.AddParsedFiles(indexedFiles)
	if err != nil {
		logger.Error("Could not update the list of parsed files")
	}
}

//isIPv4 checks if an ip is ipv4
func isIPv4(address string) bool {
	return strings.Count(address, ":") < 2
}

//ipv4ToBinary generates binary representations of the IPv4 addresses
func ipv4ToBinary(ipv4 net.IP) int64 {
	return int64(binary.BigEndian.Uint32(ipv4[12:16]))
}
