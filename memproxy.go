/**
 * Memproxy is a proxy for memcached that will split the data input
 * into fixed-size chunks for storage. It will reassemble the data
 * on retrieval with set.
 */
package main

import "bufio"
import "bytes"
import "encoding/binary"
import "errors"
import "fmt"
import "io"
import "math"
import "net"
import "strconv"
import "strings"

const verbose = false
const CHUNK_SIZE = 1024

var MISS error

type SetCmdLine struct {
    cmd     string
    key     string
    flags   int
    exptime string
    length  int
}

type GetCmdLine struct {
    cmd string
    keys []string
}

const METADATA_SIZE = 16
type Metadata struct {
    Length    int32
    OrigFlags int32
    NumChunks int32
    ChunkSize int32
}

func main() {
    // create miss error value
    MISS = errors.New("Cache miss")
    
    server, err := net.Listen("tcp", ":11212")
    
    if err != nil { print(err.Error()) }
    
    for {
        remote, err := server.Accept()
        
        if err != nil {
            fmt.Println(err.Error())
            continue
        }
        
        local, err := net.Dial("tcp", ":11211")
        
        if err != nil {
            fmt.Println(err.Error())
            continue
        }
        
        go handleConnection(remote, local)
    }
}

// <command name> <key> <flags> <exptime> <bytes> [noreply]\r\n
func handleConnection(remote net.Conn, local net.Conn) {
    remoteReader := bufio.NewReader(remote)
    remoteWriter := bufio.NewWriter(remote)
    localReader  := bufio.NewReader(local)
    localWriter  := bufio.NewWriter(local)
    
    for {
        data, err := remoteReader.ReadString('\n')
        
        data = strings.TrimSpace(data)
        
        if err != nil {
            if err == io.EOF {
                fmt.Println("Connection closed")
            } else {
                fmt.Println(err.Error())
            }
            remote.Close()
            return
        }
        
        clParts := strings.Split(data, " ")
        
        switch clParts[0] {
            case "set":
                length, err := strconv.Atoi(strings.TrimSpace(clParts[4]))
                
                if err != nil {
                    print(err.Error())
                    fmt.Fprintf(remoteWriter, "CLIENT_ERROR length is not a valid integer")
                    remote.Close()
                }
                
                flags, err := strconv.Atoi(strings.TrimSpace(clParts[2]))
                
                if err != nil {
                    print(err.Error())
                    fmt.Fprintf(remoteWriter, "CLIENT_ERROR length is not a valid integer")
                    remote.Close()
                }
                
                cmd := SetCmdLine {
                    cmd:     clParts[0],
                    key:     clParts[1],
                    flags:   flags,
                    exptime: clParts[3],
                    length:  length,
                }
                
                err = handleSet(cmd, remoteReader, remoteWriter, localReader, localWriter)
                
                if err != nil {
                    print(err.Error())
                    fmt.Fprintf(remoteWriter, "ERROR could not process command")
                    remote.Close()
                }
                
            case "get":
                cmd := GetCmdLine {
                    cmd:  clParts[0],
                    keys: clParts[1:],
                }
                
                err = handleGet(cmd, remoteReader, remoteWriter, localReader, localWriter)
                
                if err != nil {
                    print(err.Error())
                    fmt.Fprintf(remoteWriter, "ERROR could not process command")
                    remote.Close()
                }
        }
    }
}

func handleSet(cmd SetCmdLine, remoteReader *bufio.Reader, remoteWriter *bufio.Writer,
                                localReader *bufio.Reader,  localWriter *bufio.Writer) error {
    // Read in the data from the remote connection
    buf := make([]byte, cmd.length)
    err := readDataIntoBuf(remoteReader, buf)
    
    numChunks := int(math.Ceil(float64(cmd.length) / float64(CHUNK_SIZE)))
    
    metaKey := makeMetaKey(cmd.key)
    metaData := Metadata {
        Length:    int32(cmd.length),
        NumChunks: int32(numChunks),
        ChunkSize: CHUNK_SIZE,
    }
    
    if verbose {
        fmt.Println("metaKey:", metaKey)
        fmt.Println("numChunks:", numChunks)
    }
    
    metaDataBuf := new(bytes.Buffer)
    binary.Write(metaDataBuf, binary.LittleEndian, metaData)
    
    if verbose {
        fmt.Printf("% x", metaDataBuf.Bytes())
    }
    
    // Write metadata key
    localCmd := makeSetCommand(metaKey, cmd.exptime, METADATA_SIZE)
    err = setLocal(localWriter, localCmd, metaDataBuf.Bytes())
    if err != nil { return err }
    
    // Read server's response
    // TODO: Error handling of ERROR response
    response, err := localReader.ReadString('\n')
    
    if verbose { fmt.Println(response) }
    
    // Write all the data chunks
    // TODO: Clean up if a data chunk write fails
    // Failure can mean the write failing at the I/O level
    // or at the memcached level, e.g. response == ERROR
    for i := 0; i < numChunks; i++ {
        // Build this chunk's key
        key := makeChunkKey(cmd.key, i)
        
        if verbose { fmt.Println(key) }
        
        // indices for slicing, end exclusive
        start, end := sliceIndices(i, cmd.length)
        
        chunkBuf := buf[start:end]
        
        // Pad the data to always be CHUNK_SIZE
        if (end-start) < CHUNK_SIZE {
            padding := CHUNK_SIZE - (end-start)
            padtext := bytes.Repeat([]byte{byte(0)}, padding)
            chunkBuf = append(chunkBuf, padtext...)
        }
        
        // Write the key
        localCmd = makeSetCommand(key, cmd.exptime, CHUNK_SIZE)
        err = setLocal(localWriter, localCmd, chunkBuf)
        if err != nil { return err }
        
        // Read server's response
        // TODO: Error handling of ERROR response from memcached
        response, _ := localReader.ReadString('\n')
        
        if verbose { fmt.Println(response) }
    }
    
    // TODO: Error handling for less bytes
    //numWritten, err := writer.WriteString("STORED\r\n")
    _, err = remoteWriter.WriteString("STORED\r\n")
    if err != nil { return err }
    
    err = remoteWriter.Flush()
    if err != nil { return err }
    
    if verbose { fmt.Println("stored!") }
    return nil
}

func handleGet(cmd GetCmdLine, remoteReader *bufio.Reader, remoteWriter *bufio.Writer,
                                localReader *bufio.Reader,  localWriter *bufio.Writer) error {
    // read index
    // make buf
    // for numChunks do
    //   read chunk, append to buffer
    // send response
    
    outer: for _, key := range cmd.keys {
        metaKey := makeMetaKey(key)
        
        if verbose { fmt.Println("metaKey:", metaKey) }
        
        // Read in the metadata for number of chunks, chunk size, etc.
        getCmd := makeGetCommand(metaKey)
        metaBytes := make([]byte, METADATA_SIZE)
        err := getLocalIntoBuf(localReader, localWriter, getCmd, metaBytes)
        if err != nil {
            if err == MISS {
                if verbose { fmt.Println("Cache miss because of missing metadata. Cmd:", getCmd) }
                continue outer
            }
            
            return err
        }
        
        if verbose { fmt.Printf("% x", metaBytes)}
        
        var metaData Metadata
        binary.Read(bytes.NewBuffer(metaBytes), binary.LittleEndian, &metaData)
        
        if verbose { fmt.Println("Metadata:", metaData) }
        
        // Retrieve all the data from memcached
        dataBuf := make([]byte, metaData.Length)
        
        for i := 0; i < int(metaData.NumChunks); i++ {
            if verbose { fmt.Println("CHUNK", i) }
            chunkKey := makeChunkKey(key, i)
            
            // indices for slicing, end exclusive
            // TODO: pass chunk size
            start, end := sliceIndices(i, int(metaData.Length))
            
            if verbose { fmt.Println("start:", start, "| end:", end) }
            
            // Get the data directly into our buf
            chunkBuf := dataBuf[start:end]
            getCmd = makeGetCommand(chunkKey)
            err = getLocalIntoBuf(localReader, localWriter, getCmd, chunkBuf)
            
            if err != nil {
                if err == MISS {
                    if verbose { fmt.Println("Cache miss because of missing chunk. Cmd:", getCmd) }
                    continue outer
                }
                
                return err
            }
        }
        
        // Write data out to client
        // [VALUE <key> <flags> <bytes>\r\n
        // <data block>\r\n]*
        // END\r\n
        _, err = fmt.Fprintf(remoteWriter, "VALUE %s %d %d\r\n", key, metaData.OrigFlags, metaData.Length)
        if err != nil { return err }
        _, err = remoteWriter.Write(dataBuf)
        if err != nil { return err }
    }
    
    _, err := fmt.Fprintf(remoteWriter, "END\r\n")
    if err != nil { return err }
    
    remoteWriter.Flush()
    
    return nil
}

func makeMetaKey(key string) string {
    return fmt.Sprintf("%s_meta", key)
}

func makeChunkKey(key string, chunk int) string {
    return fmt.Sprintf("%s_%d", key, chunk)
}

// <command name> <key> <flags> <exptime> <bytes>\r\n
func makeSetCommand(key string, exptime string, size int) string {
    return fmt.Sprintf("set %s 0 %s %d\r\n", key, exptime, size)
}

// get(s) <key>*\r\n
// TODO: batch get
func makeGetCommand(key string) string {
    return fmt.Sprintf("get %s\r\n", key)
}

// TODO: pass chunk size
func sliceIndices(chunkNum int, totalLength int) (int, int) {
    // Indices for slicing. End is exclusive
    start := CHUNK_SIZE * chunkNum
    end := int(math.Min(float64(start + CHUNK_SIZE), float64(totalLength)))
    
    return start, end
}

func readDataIntoBuf(reader *bufio.Reader, buf []byte) error {
    if verbose {
        fmt.Println("readDataIntoBuf")
        fmt.Println("buf length:", len(buf))
    }
    
    // TODO: real error handling for not enough bytes
    _, err := io.ReadFull(reader, buf)
    if err != nil { return err }
    
    // Consume the <buffer bytes>\r\n at the end of the data
    endData, err := reader.ReadBytes('\n')
    if err != nil { return err }
    
    if verbose { fmt.Println("End data:", endData) }
    
    return nil
}

func setLocal(localWriter *bufio.Writer, cmd string, data []byte) error {
    // Write key/cmd
    if verbose { fmt.Println("cmd:", cmd) }
    _, err := localWriter.WriteString(cmd)
    if err != nil { return err }
    
    // Write value
    _, err = localWriter.Write(data)
    if err != nil { return err }
    
    // Write data end marker
    _, err = localWriter.WriteString("\r\n")
    if err != nil { return err }
    
    localWriter.Flush()
    return nil
}

// TODO: Batch get
func getLocalIntoBuf(localReader *bufio.Reader, localWriter *bufio.Writer, cmd string, buf []byte) error {
    // Write key/cmd
    if verbose { fmt.Println("cmd:", cmd) }
    _, err := localWriter.WriteString(cmd)
    if err != nil { return err }
    localWriter.Flush()
    
    // Read and discard value header line
    line, err := localReader.ReadString('\n')
    
    if strings.TrimSpace(line) == "END" {
        if verbose { fmt.Println("Cache miss for cmd", cmd) }
        return MISS
    }
    
    if verbose { fmt.Println("Header:", string(line)) }
    
    // Read in value
    err = readDataIntoBuf(localReader, buf)
    if err != nil { return err }
    
    // consume END
    _, err = localReader.ReadBytes('\n')
    
    return nil
}