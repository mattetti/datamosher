package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <input_file>")
		return
	}

	inputFileName := os.Args[1]

	// Open the input file
	inputFile, err := os.Open(inputFileName)
	if err != nil {
		fmt.Printf("Error opening input file: %v\n", err)
		return
	}
	defer inputFile.Close()

	// Parse the MP4 file
	parsedFile, err := mp4.DecodeFile(inputFile) //, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		fmt.Printf("Error parsing MP4 file: %v\n", err)
		return
	}

	// Extract I-frames and their timing, and modify their content to be null except the first one
	var iFrameTimes []float64
	firstIFrameFound := false

	for _, track := range parsedFile.Moov.Traks {
		if track.Mdia.Hdlr.HandlerType == "vide" {
			// This is a video track
			stbl := track.Mdia.Minf.Stbl
			mdhd := track.Mdia.Mdhd
			timeScale := float64(mdhd.Timescale)

			for i := 0; i < len(stbl.Stsz.SampleSize); i++ {
				sampleNr := i + 1
				// IDR frame
				if stbl.Stss != nil {

					// i-frame
					if stbl.Stss.IsSyncSample(uint32(sampleNr)) {
						// get the frame time:
						decTime, _ := stbl.Stts.GetDecodeTime(uint32(sampleNr))
						var cto int32 = 0
						if stbl.Ctts != nil {
							cto = stbl.Ctts.GetCompositionTimeOffset(uint32(sampleNr))
						}
						time := float64(decTime+uint64(cto)) / timeScale
						iFrameTimes = append(iFrameTimes, time)

						if firstIFrameFound {
							// Replace I-frame NAL unit data with null bytes
							replaceNALUnitData(parsedFile, stbl, sampleNr, inputFile)
						} else {
							firstIFrameFound = true
						}
					} else {
						processNonIDRFrame(parsedFile, stbl, sampleNr, inputFile, timeScale)
					}

				}
			}
		}
	}

	// Print I-frame timings
	// fmt.Println("I-frame timings (in seconds):")
	// for _, time := range iFrameTimes {
	// 	fmt.Printf("%.3f\n", time)
	// }

	// Rewrite the MP4 file (for simplicity, we write to a new file)
	outputFileName := "output.mp4"
	outputFile, err := os.Create(outputFileName)
	if err != nil {
		fmt.Printf("Error creating output file: %v\n", err)
		return
	}
	defer outputFile.Close()

	err = parsedFile.Encode(outputFile)
	if err != nil {
		fmt.Printf("Error writing MP4 file: %v\n", err)
		return
	}

	fmt.Printf("MP4 file rewritten successfully to %s\n", outputFileName)
}

func getChunkOffset(stbl *mp4.StblBox, chunkNr int) int64 {
	if stbl.Stco != nil {
		return int64(stbl.Stco.ChunkOffset[chunkNr-1])
	}
	if stbl.Co64 != nil {
		return int64(stbl.Co64.ChunkOffset[chunkNr-1])
	}
	panic("Neither stco nor co64 is set")
}

// replaceNALUnitData replaces the NAL unit data in the mdat box
func replaceNALUnitData(parsedFile *mp4.File, stbl *mp4.StblBox, sampleNr int, rs io.ReadSeeker) error {

	mdat := parsedFile.Mdat
	mdatPayloadStart := mdat.PayloadAbsoluteOffset()

	var avcSPS *avc.SPS
	var err error
	var codec string

	if stbl.Stsd.AvcX != nil {
		codec = "avc"
		if stbl.Stsd.AvcX.AvcC != nil {
			avcSPS, err = avc.ParseSPSNALUnit(stbl.Stsd.AvcX.AvcC.SPSnalus[0], true)
			if err != nil {
				return fmt.Errorf("error parsing SPS: %s", err)
			}
		}
	} else if stbl.Stsd.HvcX != nil {
		codec = "hevc"
	}

	chunkNr, sampleNrAtChunkStart, err := stbl.Stsc.ChunkNrFromSampleNr(sampleNr)
	if err != nil {
		return err
	}
	offset := getChunkOffset(stbl, chunkNr)
	for sNr := sampleNrAtChunkStart; sNr < sampleNr; sNr++ {
		offset += int64(stbl.Stsz.GetSampleSize(sNr))
	}
	size := stbl.Stsz.GetSampleSize(sampleNr)
	pts := getSampleTime(stbl, sampleNr)
	// Next find sample bytes as slice in mdat
	offsetInMdatData := uint64(offset) - mdatPayloadStart
	sample := mdat.Data[offsetInMdatData : offsetInMdatData+uint64(size)]

	nalus, err := avc.GetNalusFromSample(sample)
	if err != nil {
		return err
	}

	// offsets of the nalus in the sample data.
	switch codec {
	case "avc", "h.264", "h264":
		if avcSPS == nil {
			fmt.Println("No SPS found in the avcC box, trying to find it in the sample data")
			for _, nalu := range nalus {
				if avc.GetNaluType(nalu[0]) == avc.NALU_SPS {
					avcSPS, err = avc.ParseSPSNALUnit(nalu, true)
					if err != nil {
						return fmt.Errorf("error parsing SPS: %s", err)
					}
				}
			}
		}

		spsMap := make(map[uint32]*avc.SPS, 1)
		for _, spsNalu := range stbl.Stsd.AvcX.AvcC.SPSnalus {
			sps, err := avc.ParseSPSNALUnit(spsNalu, true)
			if err != nil {
				return fmt.Errorf("error parsing SPS: %s", err)
			}
			spsMap[uint32(sps.ParameterID)] = sps
		}
		ppsMap := make(map[uint32]*avc.PPS, 1)
		for _, ppsNalu := range stbl.Stsd.AvcX.AvcC.PPSnalus {
			pps, err := avc.ParsePPSNALUnit(ppsNalu, spsMap)
			if err != nil {
				return fmt.Errorf("error parsing PPS: %s", err)
			}
			ppsMap[uint32(pps.PicParameterSetID)] = pps
		}
		err = printAVCNalus(avcSPS, nalus, sampleNr, pts)

		nullifyIDR(nalus, mdat, offsetInMdatData, spsMap, ppsMap)

	case "hevc", "h.265", "h265":
		fmt.Println("HEVC not supported yet")
		return errors.New("HEVC not supported yet")
	default:
		return fmt.Errorf("unsupported codec %s", codec)
	}

	return nil
}

func getSampleTime(stbl *mp4.StblBox, sampleNr int) uint64 {
	decTime, _ := stbl.Stts.GetDecodeTime(uint32(sampleNr))
	var cto int32 = 0
	if stbl.Ctts != nil {
		cto = stbl.Ctts.GetCompositionTimeOffset(uint32(sampleNr))
	}
	return decTime + uint64(cto)
}

func nullifyIDR(nalus [][]byte, mdat *mp4.MdatBox, offsetInMdat uint64,
	spsMap map[uint32]*avc.SPS, ppsMap map[uint32]*avc.PPS) {
	// Implement the nullifyIDR function here.
	offset := 0
	for _, nalu := range nalus {
		sliceSize := len(nalu) + 4
		nType := avc.GetNaluType(nalu[0])
		fmt.Println("Nalu type:", nType)
		if nType == avc.NALU_IDR {

			// sliceHeader, err := avc.ParseSliceHeader(nalu, spsMap, ppsMap)
			// if err != nil {
			// 	panic(err)
			// }
			// fmt.Printf("Slice %#v\n", sliceHeader)
			nullSlice := make([]byte, sliceSize)
			naluOffsetInMdat := offsetInMdat + uint64(offset)
			fmt.Println("Nullifying IDR NALU @", naluOffsetInMdat)
			copy(mdat.Data[naluOffsetInMdat:offsetInMdat+uint64(sliceSize-4)], nullSlice)
		} else if nType == avc.NALU_NON_IDR {
			// sliceHeader, err := avc.ParseSliceHeader(nalu, spsMap, ppsMap)
			// if err != nil {
			// 	panic(err)
			// }
			// fmt.Printf("Slice %#v\n", sliceHeader)
		} else if nType == avc.NALU_SEI {
			rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
			randomInt := rnd.Intn(11)
			if randomInt > 2 {
				fmt.Println("Nullifying SEI NALU")
				nullSlice := make([]byte, sliceSize)
				copy(mdat.Data[offsetInMdat:offsetInMdat+uint64(sliceSize-4)], nullSlice)
			}
		}
	}
}

func processNonIDRFrame(parsedFile *mp4.File, stbl *mp4.StblBox, sampleNr int, rs io.ReadSeeker, timeScale float64) error {
	var err error

	mdat := parsedFile.Mdat
	mdatPayloadStart := mdat.PayloadAbsoluteOffset()

	// var avcSPS *avc.SPS
	// var codec string

	// if stbl.Stsd.AvcX != nil {
	// 	codec = "avc"
	// 	if stbl.Stsd.AvcX.AvcC != nil {
	// 		avcSPS, err = avc.ParseSPSNALUnit(stbl.Stsd.AvcX.AvcC.SPSnalus[0], true)
	// 		if err != nil {
	// 			return fmt.Errorf("error parsing SPS: %s", err)
	// 		}
	// 	}
	// } else if stbl.Stsd.HvcX != nil {
	// 	codec = "hevc"
	// }
	// if codec != "avc" {
	// 	return nil
	// }

	chunkNr, sampleNrAtChunkStart, err := stbl.Stsc.ChunkNrFromSampleNr(sampleNr)
	if err != nil {
		return err
	}
	offset := getChunkOffset(stbl, chunkNr)
	for sNr := sampleNrAtChunkStart; sNr < sampleNr; sNr++ {
		offset += int64(stbl.Stsz.GetSampleSize(sNr))
	}
	size := stbl.Stsz.GetSampleSize(sampleNr)
	pts := getSampleTime(stbl, sampleNr)
	timeInSecs := float64(pts) / timeScale

	offsetInMdatData := uint64(offset) - mdatPayloadStart
	sample := mdat.Data[offsetInMdatData : offsetInMdatData+uint64(size)]

	nalus, err := avc.GetNalusFromSample(sample)
	if err != nil {
		return err
	}

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	for _, nalu := range nalus {
		var offsetInMdat uint64
		naluType := avc.GetNaluType(nalu[0])
		sliceSize := len(nalu) + 4

		if naluType == avc.NALU_NON_IDR {
			sliceType, _ := avc.GetSliceTypeFromNALU(nalu)

			// Generate a random int between 0 and 10
			randomInt := rnd.Intn(11)

			// If the random int is more than x, nullify the frame
			if randomInt > 0 {
				fmt.Printf("Nullifying %s frame @ %.2f\n", sliceType, timeInSecs)
				nullSlice := make([]byte, sliceSize)
				copy(mdat.Data[offsetInMdat:offsetInMdat+uint64(sliceSize)], nullSlice)
			} else {
				fmt.Printf("%s frame pts: %.2f size: %d\n", sliceType, timeInSecs, len(nalu))
			}
			// Else do nothing
		} else {
			fmt.Println("Not a non-IDR frame but", naluType)
		}
		offsetInMdat += uint64(sliceSize)
	}

	return err
}

func printAVCNalus(avcSPS *avc.SPS, nalus [][]byte, nr int, pts uint64) error {
	msg := ""
	totLen := 0
	offsets := []int{}
	offset := 0
	for i, nalu := range nalus {
		offsets = append(offsets, offset)
		totLen += 4 + len(nalu)
		if i > 0 {
			msg += ","
		}
		naluType := avc.GetNaluType(nalu[0])
		imgType := ""
		var err error
		switch naluType {
		case avc.NALU_SPS:
			avcSPS, err = avc.ParseSPSNALUnit(nalu, true)
			if err != nil {
				return fmt.Errorf("error parsing SPS: %s", err)
			}
		case avc.NALU_NON_IDR, avc.NALU_IDR:
			sliceType, err := avc.GetSliceTypeFromNALU(nalu)
			if err == nil {
				imgType = fmt.Sprintf("[%s] ", sliceType)
			}
		case avc.NALU_SEI:
			//
		}
		msg += fmt.Sprintf(" %s %s(%dB)", naluType, imgType, len(nalu))
		offset += 4 + len(nalu)
	}
	fmt.Printf("Sample %d, pts=%d (%dB):%s\n", nr, pts, totLen, msg)
	for i, nalu := range nalus {
		fmt.Printf("  NALU %d: %s - from %d to %d\n", i, avc.GetNaluType(nalu[0]),
			offsets[i], offsets[i]+len(nalu)+4)
	}
	return nil
}

// GetSampleFromNalus - convert a slice of NAL units to a sample
func GetSampleFromNalus(nalus [][]byte) ([]byte, error) {
	var buf bytes.Buffer

	for _, nalu := range nalus {
		// Write the NAL unit length
		naluLength := uint32(len(nalu))
		err := binary.Write(&buf, binary.BigEndian, naluLength)
		if err != nil {
			return nil, err
		}

		// Write the NAL unit data
		_, err = buf.Write(nalu)
		if err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}
