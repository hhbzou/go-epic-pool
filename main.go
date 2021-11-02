package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type Message struct {
	Version string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  interface{}     `json:"result"`
	Error   interface{}     `json:"error"`
}

type JobParams struct {
	// Algorithm of the block.
	Algorithm string `json:"algorithm"`
	// Slice of `[algorithm, difficulty]` pairs.
	BlockDifficulty [][]interface{} `json:"block_difficulty"`
	// This is share difficulty - we don't care about it.
	Difficulty [][]interface{} `json:"difficulty"`
	// Value at `[0][2]` is a `seed_hash`.
	Epochs [][]interface{} `json:"epochs"`
	Height uint64          `json:"height"`
	JobID  int             `json:"job_id"`
	PrePow string          `json:"pre_pow"`
}

func findDifficulty(diffs [][]interface{}, algo string) uint64 {
	for _, v := range diffs {
		if name, _ := v[0].(string); name == algo {
			return uint64(v[1].(float64))
		}
	}
	return 1e9
}

func (p *JobParams) difficulty() uint64 {
	return findDifficulty(p.Difficulty, "randomx")
}

func (p *JobParams) blockDifficulty() uint64 {
	return findDifficulty(p.BlockDifficulty, "randomx")
}

func (p *JobParams) seedHash() string {
	return hex.EncodeToString(interfacesToBytes(p.Epochs[0][2].([]interface{})))
}

type SubmitParams struct {
	Height uint64 `json:"height"`
	JobID  int    `json:"job_id"`
	Nonce  uint64 `json:"nonce"`
	Pow    struct {
		RandomX [32]byte
	} `json:"pow"`
}

type XmrigEvent struct {
	Version string      `json:"jsonrpc"`
	ID      uint64      `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type XmrigMessage struct {
	Version string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type XmrigResponse struct {
	Version string      `json:"jsonrpc"`
	ID      uint64      `json:"id"`
	Error   interface{} `json:"error"`
	Method  string      `json:"method,omitempty"` // TODO: maybe remove
	Result  interface{} `json:"result"`
}

type XmrigJob struct {
	Blob       string `json:"blob"`
	JobID      string `json:"job_id"`
	Difficulty uint64 `json:"difficulty"`
	Algorithm  string `json:"algo"`
	Height     uint64 `json:"height"`
	SeedHash   string `json:"seed_hash,omitempty"`
}

type XmrigSubmitRequest struct {
	ID    string `json:"id"`
	JobID string `json:"job_id"`
	// Nonce encoded as hex.
	Nonce string `json:"nonce"`
	// Result hash.
	Result    string `json:"result"`
	Algorithm string `json:"algo"`
}

func interfacesToBytes(input []interface{}) (res []byte) {
	res = make([]byte, 32)
	for i := 0; i < 32; i++ {
		res[i] = byte(input[i].(float64))
	}
	return
}

func preparePrePow(prePowHex string) string {
	prePow, _ := hex.DecodeString(prePowHex)
	zeros := make([]byte, 8)
	prePow = append(prePow, zeros...)
	return hex.EncodeToString(prePow)
}

func readNodeJob(body []byte) (job *XmrigJob, err error) {
	var msg Message
	err = json.Unmarshal(body, &msg)
	if err != nil {
		return
	}
	if msg.Error != nil {
		log.Printf("Error: %#v", msg)
		return
	}
	if msg.Method != "job" {
		//log.Printf("Node MSG: %#v", msg)
		return
	}
	var jobParams JobParams
	err = json.Unmarshal(msg.Params, &jobParams)
	if err != nil {
		return nil, err
	}
	//log.Printf("Node JOB: %#v", jobParams)
	if jobParams.Algorithm != "randomx" {
		return &XmrigJob{
			Blob:       preparePrePow(jobParams.PrePow),
			JobID:      fmt.Sprintf("%d", jobParams.JobID),
			Difficulty: jobParams.blockDifficulty(),
			Algorithm:  "pause",
			Height:     jobParams.Height,
			SeedHash:   jobParams.seedHash(),
		}, nil
	}
	return &XmrigJob{
		Blob:       preparePrePow(jobParams.PrePow),
		JobID:      fmt.Sprintf("%d", jobParams.JobID),
		Difficulty: jobParams.blockDifficulty() - 1,
		Algorithm:  "rx/epic",
		Height:     jobParams.Height,
		SeedHash:   jobParams.seedHash(),
	}, nil
}

func connectNode(jobsChan chan<- *XmrigJob, solutionsChan <-chan XmrigSubmitRequest, currentJob *currentJob) (err error) {
	conn, err := net.Dial("tcp", "127.0.0.1:3416")
	if err != nil {
		return
	}
	defer conn.Close()
	writer := bufio.NewWriter(conn)

	ctx, cancel := context.WithCancel(context.Background())
	// start reading in a goroutine
	go func() {
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				cancel()
				return
			}
			job, err := readNodeJob(line)
			if err != nil {
				log.Printf("Error reading message %s error=%v", line, err)
			}
			if job != nil {
				log.Printf("Received %s job on height %d diff=%d", job.Algorithm, job.Height, job.Difficulty)
				if currentJob.setJob(job) {
					jobsChan <- job
				}
			}
		}
	}()

	var id uint64 = 2
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case solution := <-solutionsChan:
			log.Printf("Got solution: %#v", solution)
			currentJob.incrBlocks()
			jobID, _ := strconv.Atoi(solution.JobID)
			height := currentJob.getHeight()
			nonceBytes, err := hex.DecodeString(solution.Nonce)
			if err != nil {
				log.Fatal(err)
			}
			nonce := binary.BigEndian.Uint32(nonceBytes)
			pow := struct{ RandomX [32]byte }{}
			_, err = hex.Decode(pow.RandomX[:], []byte(solution.Result))
			if err != nil {
				log.Fatal(err)
			}
			body, err := json.Marshal(SubmitParams{
				JobID:  jobID,
				Height: height,
				Nonce:  uint64(nonce),
				Pow:    pow,
			})
			if err != nil {
				log.Fatal(err)
			}
			err = writeJSON(writer, Message{
				Version: "2.0",
				ID:      fmt.Sprintf("%d", id),
				Method:  "submit",
				Params:  body,
			})
			if err != nil {
				log.Printf("Error writing solution: %v", err)
			}
			id += 1
		}
	}
}

func writeJSON(writer *bufio.Writer, value interface{}) (err error) {
	body, err := json.Marshal(value)
	if err != nil {
		log.Fatal(err)
	}
	_, err = writer.Write(body)
	if err != nil {
		return
	}
	_, err = writer.Write([]byte{'\n'})
	if err != nil {
		return
	}
	//log.Printf("Wrote response: %s", body)
	return writer.Flush()
}

func handleConn(conn net.Conn, connJobs <-chan *XmrigJob, solutionsChan chan<- XmrigSubmitRequest, currentJob *currentJob) {
	defer func() {
		conn.Close()
		currentJob.decrConns()
	}()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	responses := make(chan XmrigResponse, 10)
	loginJob := <-connJobs
	var msg XmrigMessage
	go func() {
		for {
			select {
			case job := <-connJobs:
				event := XmrigEvent{
					Version: "2.0",
					Method:  "job",
					Params:  job,
				}
				if err := writeJSON(writer, event); err != nil {
					return
				}
			case resp := <-responses:
				if err := writeJSON(writer, resp); err != nil {
					return
				}
			}
		}
	}()

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		//log.Printf("Received message: %s", line)
		err = json.Unmarshal(line, &msg)
		if err != nil {
			log.Printf("Error :%v", err)
			continue
		}
		if msg.Method == "keepalived" {
			responses <- XmrigResponse{
				Version: msg.Version,
				ID:      msg.ID,
				Method:  msg.Method,
				Result: struct {
					Status string `json:"status"`
				}{Status: "KEEPALIVED"},
			}
		}
		if msg.Method == "login" {
			responses <- XmrigResponse{
				Version: msg.Version,
				ID:      msg.ID,
				Result: struct {
					ID         string    `json:"id"`
					Job        *XmrigJob `json:"job"`
					Extensions []string  `json:"extensions"`
					Status     string    `json:"status"`
				}{ID: "crackcomm", Extensions: []string{"algo", "keepalive", "connect"}, Status: "OK", Job: loginJob},
			}
		}
		if msg.Method != "submit" {
			continue
		}
		var submitRequest XmrigSubmitRequest
		err = json.Unmarshal(msg.Params, &submitRequest)
		if err != nil {
			log.Printf("Submit error: %v", err)
			continue
		}
		solutionsChan <- submitRequest
		responses <- XmrigResponse{
			Version: msg.Version,
			ID:      msg.ID,
			Method:  msg.Method,
			Result: struct {
				Status string `json:"status"`
			}{Status: "OK"},
		}
	}
}

type currentJob struct {
	job         *XmrigJob
	foundBlocks uint64
	conns       uint64
	lock        *sync.RWMutex
}

func (currentJob *currentJob) getHeight() uint64 {
	currentJob.lock.RLock()
	defer currentJob.lock.RUnlock()
	if currentJob.job == nil {
		return 0
	}
	return currentJob.job.Height
}

func (currentJob *currentJob) getJob() *XmrigJob {
	currentJob.lock.RLock()
	defer currentJob.lock.RUnlock()
	return currentJob.job
}

func (currentJob *currentJob) getFoundBlocks() uint64 {
	currentJob.lock.RLock()
	defer currentJob.lock.RUnlock()
	return currentJob.foundBlocks
}

func (currentJob *currentJob) activeConns() uint64 {
	currentJob.lock.RLock()
	defer currentJob.lock.RUnlock()
	return currentJob.conns
}

func (currentJob *currentJob) decrConns() {
	currentJob.lock.Lock()
	currentJob.conns -= 1
	currentJob.lock.Unlock()
}

func (currentJob *currentJob) incrConns() {
	currentJob.lock.Lock()
	currentJob.conns += 1
	currentJob.lock.Unlock()
}

func (currentJob *currentJob) incrBlocks() {
	currentJob.lock.Lock()
	currentJob.foundBlocks += 1
	currentJob.lock.Unlock()
}

func (currentJob *currentJob) setJob(job *XmrigJob) (res bool) {
	currentJob.lock.Lock()
	if currentJob.job == nil || !(currentJob.job.Algorithm == "pause" && job.Algorithm == "pause") {
		res = true
	}
	currentJob.job = job
	currentJob.lock.Unlock()
	return
}

func main() {
	currentJob := &currentJob{lock: new(sync.RWMutex)}
	jobsChan := make(chan *XmrigJob, 10)
	solutionsChan := make(chan XmrigSubmitRequest, 10)
	// connect to epic node to listen for jobs
	go func() {
		for {
			if err := connectNode(jobsChan, solutionsChan, currentJob); err != nil {
				log.Printf("Node error: %v", err)
				time.Sleep(time.Second)
			}
		}
	}()

	http.HandleFunc("/blocks", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Mined %d blocks.\nActive connections: %d.\n", currentJob.getFoundBlocks(), currentJob.activeConns())
	})

	go func() {
		http.ListenAndServe(":4441", nil)
	}()

	// create broadcasting goroutine
	connsChan := make(chan chan *XmrigJob, 10) // channel of channels
	go func() {
		var counter uint64 = 0
		channels := map[uint64]chan *XmrigJob{}
		for {
			select {
			case newChan := <-connsChan:
				channels[counter] = newChan
				counter += 1
			case job := <-jobsChan:
				for id, ch := range channels {
					select {
					case ch <- job:
					default:
						delete(channels, id)
					}
				}

			}
		}
	}()

	listener, err := net.Listen("tcp", "0.0.0.0:33416")
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		currentJob.incrConns()
		connJobs := make(chan *XmrigJob, 1)
		connsChan <- connJobs
		job := currentJob.getJob()
		if job != nil {
			connJobs <- job
		}
		go handleConn(conn, connJobs, solutionsChan, currentJob)
	}
}
