package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

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
				if stbl.Stss != nil && stbl.Stss.IsSyncSample(uint32(sampleNr)) {
					// This sample is an I-frame
					decTime, _ := stbl.Stts.GetDecodeTime(uint32(sampleNr))
					time := float64(decTime) / timeScale
					iFrameTimes = append(iFrameTimes, time)

					if firstIFrameFound {
						// Replace I-frame NAL unit data with null bytes
						replaceNALUnitData(parsedFile, stbl, sampleNr, inputFile)
					} else {
						firstIFrameFound = true
					}
				}
			}
		}
	}

	// Print I-frame timings
	fmt.Println("I-frame timings (in seconds):")
	for _, time := range iFrameTimes {
		fmt.Printf("%.3f\n", time)
	}

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

	// nrSamples := stbl.Stsz.SampleNumber
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
	decTime, _ := stbl.Stts.GetDecodeTime(uint32(sampleNr))
	var cto int32 = 0
	if stbl.Ctts != nil {
		cto = stbl.Ctts.GetCompositionTimeOffset(uint32(sampleNr))
	}
	// Next find sample bytes as slice in mdat
	offsetInMdatData := uint64(offset) - mdatPayloadStart
	sample := mdat.Data[offsetInMdatData : offsetInMdatData+uint64(size)]

	nalus, err := avc.GetNalusFromSample(sample)
	if err != nil {
		return err
	}

	switch codec {
	case "avc", "h.264", "h264":
		if avcSPS == nil {
			for _, nalu := range nalus {
				if avc.GetNaluType(nalu[0]) == avc.NALU_SPS {
					avcSPS, err = avc.ParseSPSNALUnit(nalu, true)
					if err != nil {
						return fmt.Errorf("error parsing SPS: %s", err)
					}
				}
			}
		}

		err = printAVCNalus(avcSPS, nalus, sampleNr, decTime+uint64(cto))
		// TODO: Modify the NAL unit data here, nullify the i frame data.
		// call GetSampleFromNalus(nalus) to get the new sample data
		tempSlice := make([]byte, size)
		copy(mdat.Data[offsetInMdatData+1:offsetInMdatData+uint64(size-1)], tempSlice)

	case "hevc", "h.265", "h265":
		fmt.Println("HEVC not supported yet")
		return errors.New("HEVC not supported yet")
	default:
		return fmt.Errorf("unsupported codec %s", codec)
	}

	return nil
}

func printAVCNalus(avcSPS *avc.SPS, nalus [][]byte, nr int, pts uint64) error {
	msg := ""
	totLen := 0
	for i, nalu := range nalus {
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
	}
	fmt.Printf("Sample %d, pts=%d (%dB):%s\n", nr, pts, totLen, msg)
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
