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

type DatamoshConfig struct {
	DropIFramePercent float32 // 0-1 percentage of I-frames to drop
	DropPFramePercent float32
	DropBFramePercent float32
	DropSEIPercent    float32
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <input_file>")
		return
	}

	inputFileName := os.Args[1]
	inputFile, err := os.Open(inputFileName)
	if err != nil {
		fmt.Printf("Error opening input file: %v\n", err)
		return
	}
	defer inputFile.Close()

	parsedFile, err := mp4.DecodeFile(inputFile)
	if err != nil {
		fmt.Printf("Error parsing MP4 file: %v\n", err)
		return
	}

	config := DatamoshConfig{
		DropPFramePercent: .3,
		DropBFramePercent: .2,
		DropIFramePercent: .9,
		DropSEIPercent:    0.4,
	}

	_, err = processVideoTracks(parsedFile, inputFile, config)
	if err != nil {
		fmt.Printf("Error processing video tracks: %v\n", err)
		return
	}

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

func processVideoTracks(parsedFile *mp4.File, inputFile *os.File, config DatamoshConfig) ([]float64, error) {
	var iFrameTimes []float64

	for _, track := range parsedFile.Moov.Traks {
		if track.Mdia.Hdlr.HandlerType == "vide" {
			iFrameTimes = append(iFrameTimes, processVideoTrack(parsedFile, track, inputFile, config)...)
		}
	}
	return iFrameTimes, nil
}

func processVideoTrack(parsedFile *mp4.File, track *mp4.TrakBox, inputFile *os.File, config DatamoshConfig) []float64 {
	firstIFrameFound := false
	stbl := track.Mdia.Minf.Stbl
	mdhd := track.Mdia.Mdhd
	timeScale := float64(mdhd.Timescale)
	var iFrameTimes []float64

	for i := 0; i < len(stbl.Stsz.SampleSize); i++ {
		sampleNr := i + 1
		if stbl.Stss != nil {
			time := float64(getSampleTime(stbl, sampleNr)) / timeScale
			if stbl.Stss.IsSyncSample(uint32(sampleNr)) {
				iFrameTimes = append(iFrameTimes, time)
				if firstIFrameFound {
					err := processFrame(parsedFile, stbl, sampleNr, inputFile, config, "I")
					if err != nil {
						fmt.Printf("Error processing I-frame: %v\n", err)
					}
				} else {
					firstIFrameFound = true
				}
			} else if firstIFrameFound && time > 0.5 {
				err := processFrame(parsedFile, stbl, sampleNr, inputFile, config, "P")
				if err != nil {
					fmt.Printf("Error processing non-IDR frame: %v\n", err)
				}
			}
		}
	}
	return iFrameTimes
}

func processFrame(parsedFile *mp4.File, stbl *mp4.StblBox, sampleNr int, rs io.ReadSeeker, config DatamoshConfig, frameType string) error {
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
		spsMap, ppsMap, err := parseSPSAndPPS(stbl.Stsd.AvcX.AvcC)
		if err != nil {
			return err
		}
		err = printAVCNalus(avcSPS, nalus, sampleNr, pts)
		if err != nil {
			return err
		}
		applyDatamosh(nalus, mdat, offsetInMdatData, config, frameType, spsMap, ppsMap)

	case "hevc", "h.265", "h265":
		return errors.New("HEVC not supported yet")
	default:
		return fmt.Errorf("unsupported codec %s", codec)
	}

	return nil
}

func parseSPSAndPPS(avcC *mp4.AvcCBox) (map[uint32]*avc.SPS, map[uint32]*avc.PPS, error) {
	spsMap := make(map[uint32]*avc.SPS, 1)
	for _, spsNalu := range avcC.SPSnalus {
		sps, err := avc.ParseSPSNALUnit(spsNalu, true)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing SPS: %s", err)
		}
		spsMap[uint32(sps.ParameterID)] = sps
	}
	ppsMap := make(map[uint32]*avc.PPS, 1)
	for _, ppsNalu := range avcC.PPSnalus {
		pps, err := avc.ParsePPSNALUnit(ppsNalu, spsMap)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing PPS: %s", err)
		}
		ppsMap[uint32(pps.PicParameterSetID)] = pps
	}
	return spsMap, ppsMap, nil
}

func getSampleTime(stbl *mp4.StblBox, sampleNr int) uint64 {
	decTime, _ := stbl.Stts.GetDecodeTime(uint32(sampleNr))
	var cto int32
	if stbl.Ctts != nil {
		cto = stbl.Ctts.GetCompositionTimeOffset(uint32(sampleNr))
	}
	return decTime + uint64(cto)
}

func applyDatamosh(nalus [][]byte, mdat *mp4.MdatBox, offsetInMdat uint64, config DatamoshConfig, frameType string, spsMap map[uint32]*avc.SPS, ppsMap map[uint32]*avc.PPS) {
	offset := 0
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	for _, nalu := range nalus {
		randomF := rnd.Float32()
		sliceSize := len(nalu)
		nType := avc.GetNaluType(nalu[0])
		percentLimit := getFrameDropPercent(config, nType, frameType)

		if randomF < percentLimit {
			nullSlice := make([]byte, sliceSize)
			naluOffsetInMdat := offsetInMdat + uint64(offset)
			copy(mdat.Data[naluOffsetInMdat+4:offsetInMdat+uint64(len(nullSlice))], nullSlice)
		}
		offset += sliceSize
	}
}

func getFrameDropPercent(config DatamoshConfig, nType avc.NaluType, frameType string) float32 {
	switch nType {
	case avc.NALU_IDR:
		return config.DropIFramePercent
	case avc.NALU_SEI:
		return config.DropSEIPercent
	default:
		if frameType == "P" {
			return config.DropPFramePercent
		}
		return config.DropBFramePercent
	}
}

func printAVCNalus(avcSPS *avc.SPS, nalus [][]byte, nr int, pts uint64) error {
	var msg string
	totLen := 0
	offsets := make([]int, len(nalus))
	offset := 0

	for i, nalu := range nalus {
		offsets[i] = offset
		totLen += 4 + len(nalu)
		naluType := avc.GetNaluType(nalu[0])
		imgType := ""
		if naluType == avc.NALU_SPS {
			var err error
			avcSPS, err = avc.ParseSPSNALUnit(nalu, true)
			if err != nil {
				return fmt.Errorf("error parsing SPS: %s", err)
			}
		} else if naluType == avc.NALU_NON_IDR || naluType == avc.NALU_IDR {
			sliceType, err := avc.GetSliceTypeFromNALU(nalu)
			if err == nil {
				imgType = fmt.Sprintf("[%s] ", sliceType)
			}
		}
		msg += fmt.Sprintf(" %s %s(%dB)", naluType, imgType, len(nalu))
		offset += 4 + len(nalu)
	}
	fmt.Printf("Sample %d, pts=%d (%dB):%s\n", nr, pts, totLen, msg)
	for i, nalu := range nalus {
		fmt.Printf("  NALU %d: %s - from %d to %d\n", i, avc.GetNaluType(nalu[0]), offsets[i], offsets[i]+len(nalu)+4)
	}
	return nil
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

func GetSampleFromNalus(nalus [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	for _, nalu := range nalus {
		naluLength := uint32(len(nalu))
		err := binary.Write(&buf, binary.BigEndian, naluLength)
		if err != nil {
			return nil, err
		}
		_, err = buf.Write(nalu)
		if err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
