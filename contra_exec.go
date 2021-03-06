package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kravitz/contra_lib/db"
	"github.com/kravitz/contra_lib/model"
	"github.com/kravitz/contra_lib/util"

	"github.com/streadway/amqp"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

const execPath = "/home/contra/exec_dir"

var srcDir = filepath.Join(execPath, "src")
var runDir = filepath.Join(execPath, "run")
var outDir = filepath.Join(execPath, "out")

type contraExecApp struct {
	clientID      string
	consoleLaunch bool
	s             *mgo.Session
	q             *amqp.Connection
}

type fileTreeState struct {
	IsDir    bool
	NodeName string
	ModTime  time.Time
	Size     int64
	Children map[string]*fileTreeState
}

func prepareExecDir() {
	os.RemoveAll(execPath)
	os.Mkdir(execPath, 0700)
	os.Mkdir(srcDir, 0700)
	os.Mkdir(runDir, 0700)
	os.Mkdir(outDir, 0700)
}

func (app *contraExecApp) retrieveFile(id string, collection string, dir string, executable bool) *model.FileDescription {
	s := app.s.Copy()
	defer s.Close()

	file, err := s.DB("tram").GridFS(collection).OpenId(bson.ObjectIdHex(id))
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	fd := &model.FileDescription{}

	file.GetMeta(fd)
	filename := filepath.Join(dir, fd.Filename)
	out, err := os.Create(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()
	io.Copy(out, file)
	if executable {
		out.Chmod(0700)
	}
	return fd
}

func guessUnpackCommand(workdir, fullFilename string) *exec.Cmd {
	ext := filepath.Ext(fullFilename)
	var cmd *exec.Cmd
	switch ext {
	case ".tar":
		cmd = exec.Command("tar", "-xf", fullFilename)
	case ".gz", ".gzip":
		cmd = exec.Command("tar", "-xzf", fullFilename)
	case ".7z", ".7zip":
		cmd = exec.Command("7za", "x", fullFilename)
	}
	if cmd != nil {
		cmd.Dir = workdir
	}
	return cmd
}

func guessPackCommand(workdir, filename, dirToPack string) *exec.Cmd {
	ext := filepath.Ext(filename)
	var cmd *exec.Cmd
	switch ext {
	case ".tar":
		cmd = exec.Command("tar", "-cf", filename, dirToPack)
	case ".gz", ".gzip":
		cmd = exec.Command("tar", "-czf", filename, dirToPack)
		// case ".7z", ".7zip":
		// cmd = exec.Command("7za", "x", fullFilename)
	}
	if cmd != nil {
		cmd.Dir = workdir
	}
	return cmd
}

func unpackData(workdir, srcDir, filename string) {
	fullFilename := filepath.Join(srcDir, filename)
	cmd := guessUnpackCommand(workdir, fullFilename)

	out, err := cmd.CombinedOutput()
	log.Println(string(out))
	if err != nil {
		log.Fatal(err)
	}
}

func diveIntoData(workdir string) string {
	wd, err := os.Open(workdir)
	if err != nil {
		log.Fatal(err)
	}
	fis, err := wd.Readdir(0)
	if err != nil {
		log.Fatal(err)
	}
	finalPath := workdir
	if len(fis) == 1 && fis[0].IsDir() {
		finalPath = diveIntoData(filepath.Join(finalPath, fis[0].Name()))
	}
	return finalPath
}

func simpleCopy(oldpath, newpath string) error {
	fd1, err := os.Open(oldpath)
	if err != nil {
		return err
	}
	defer fd1.Close()
	fd2, err := os.Create(newpath)
	if err != nil {
		return err
	}
	fd1Stat, err := fd1.Stat()
	if err != nil {
		return err
	}
	defer fd2.Close()
	io.Copy(fd2, fd1)

	fd2.Chmod(fd1Stat.Mode())
	return nil
}

func runControlScript(workdir, filename string) ([]byte, error) {
	cmd := exec.Command("/bin/bash", filepath.Join(workdir, filename))
	cmd.Dir = workdir

	out, err := cmd.CombinedOutput()

	return out, err
}

func convertToUnixLE(fullFilename string) {
	cmd := exec.Command("dos2unix", fullFilename)
	err := cmd.Run()
	if err != nil {
		log.Fatal(err)
	}
}

// TODO: share mogno and amqp init section to get rid of init order importance
func createApp() *contraExecApp {
	mongoSocket := "tram-mongo:27017"
	log.Println("Connect to mongo at:", mongoSocket)
	s, err := db.MongoInitConnect(mongoSocket)
	if err != nil {
		log.Fatal(err)
	}

	rabbitUser := util.GetenvDefault("RABBIT_USER", "guest")
	rabbitPassword := util.GetenvDefault("RABBIT_PASSWORD", "guest")
	amqpSocket := fmt.Sprintf("amqp://%v:%v@tram-rabbit:5672", rabbitUser, rabbitPassword)
	log.Println("Connect to rabbit at:", amqpSocket)
	q, err := db.RabbitInitConnect(amqpSocket)
	if err != nil {
		log.Fatal(err)
	}

	app := &contraExecApp{
		s:             s,
		q:             q,
		clientID:      os.Getenv("clientID"),
		consoleLaunch: false,
	}
	return app
}

func (app *contraExecApp) Stop() {
	app.s.Close()
	app.q.Close()
}

func placeControlScript(workdir, srcDir, filename string) string {
	workdir = diveIntoData(workdir)
	err := simpleCopy(filepath.Join(srcDir, filename), filepath.Join(workdir, filename))
	if err != nil {
		log.Fatal(err)
	}

	return workdir
}

func getFileTreeState(pth string) (fts *fileTreeState, err error) {
	f, err := os.Open(pth)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	fts = &fileTreeState{
		IsDir:    fi.IsDir(),
		ModTime:  fi.ModTime(),
		Size:     fi.Size(),
		NodeName: filepath.Base(pth),
		Children: nil,
	}

	if fi.IsDir() {
		names, err := f.Readdirnames(0)
		f.Close()
		if err != nil {
			return nil, err
		}
		if names != nil && len(names) > 0 {
			fts.Children = map[string]*fileTreeState{}
		}
		for _, name := range names {
			childPath := filepath.Join(pth, name)
			childFts, err := getFileTreeState(childPath)
			if err != nil {
				return nil, err
			}
			fts.Children[name] = childFts
		}
	}
	f.Close()
	return fts, nil
}

func findFileTreeStateChanges(dsa, dsb *fileTreeState) (diff *fileTreeState) {
	diff = nil

	if dsa.IsDir != dsb.IsDir {
		diff = dsb
	} else if dsb.IsDir {
		if dsb.Children != nil {
			diff = &fileTreeState{
				IsDir:    true,
				NodeName: dsb.NodeName,
				ModTime:  dsb.ModTime,
				Children: map[string]*fileTreeState{},
			}
			anyChildrenAdded := false
			for name, dsbChild := range dsb.Children {
				dsaChild, present := dsa.Children[name]
				if present {
					diffChild := findFileTreeStateChanges(dsaChild, dsbChild)
					if diffChild != nil {
						diff.Children[name] = diffChild
						anyChildrenAdded = true
					}
				} else {
					diff.Children[name] = dsbChild
					anyChildrenAdded = true
				}
			}
			if !anyChildrenAdded {
				diff = nil
			}
		}
	} else if dsa.ModTime != dsb.ModTime || dsa.Size != dsb.Size {
		diff = dsb
	}

	return diff
}

func treeCopy(from, to string, tree *fileTreeState) error {
	curToPath := filepath.Join(to, tree.NodeName)
	curFromPath := filepath.Join(from, tree.NodeName)
	if tree.IsDir {
		if err := os.Mkdir(curToPath, 0755); err != nil {
			return err
		}
		for _, childTree := range tree.Children {
			if err := treeCopy(curFromPath, curToPath, childTree); err != nil {
				return err
			}
		}
	} else {
		if err := simpleCopy(curFromPath, curToPath); err != nil {
			return err
		}
	}
	return nil
}

func packTree(baseDir string, outDir string, filename string, tree *fileTreeState) error {
	// outBaseDir := filepath.Base(baseDir)
	os.RemoveAll(outDir)
	os.MkdirAll(outDir, 0755)
	dirToPack := tree.NodeName
	if err := treeCopy(baseDir, outDir, tree); err != nil {
		return err
	}
	packCmd := guessPackCommand(outDir, filename, dirToPack)
	if packCmd == nil {
		log.Fatal("Pack command creation failed")
	}

	if output, err := packCmd.CombinedOutput(); err != nil {
		return errors.New(string(output))
	}

	// packSrcDir := filepath.Join(outDir, outBaseDir)
	// os.Mkdir(packSrcDir, 0755)
	return nil
}

func (app *contraExecApp) execute(dataFid, controlFid string) ([]byte, string, error) {
	prepareExecDir()
	dataFd := app.retrieveFile(dataFid, "data", srcDir, false)
	controlFd := app.retrieveFile(controlFid, "control", srcDir, true)
	convertToUnixLE(filepath.Join(srcDir, controlFd.Filename))
	unpackData(runDir, srcDir, dataFd.Filename)
	execDir := placeControlScript(runDir, srcDir, controlFd.Filename)

	ftsBefore, _ := getFileTreeState(runDir)
	s, e := runControlScript(execDir, controlFd.Filename)
	ftsAfter, _ := getFileTreeState(runDir)

	diff := findFileTreeStateChanges(ftsBefore, ftsAfter)

	var outputName string
	if diff != nil {
		arName := "output.tar.gz"
		packTree(filepath.Dir(runDir), outDir, arName, diff)
		outputName = filepath.Join(outDir, arName)
	}

	return s, outputName, e
}

func uploadOutput(s *mgo.Session, filename string) interface{} {
	gfs := db.GetGridFS(s, "output")
	outH, _ := gfs.Create("")
	defer outH.Close()

	outH.SetMeta(bson.M{"filename": filepath.Base(filename)})

	inH, _ := os.Open(filename)
	defer inH.Close()

	io.Copy(outH, inH)

	return outH.Id()
}

func (app *contraExecApp) processDelivery(delivery amqp.Delivery) {
	msg := model.TaskMsg{}
	if err := bson.Unmarshal(delivery.Body, &msg); err != nil {
		log.Fatal(err)
	}
	output, outputFilename, err := app.execute(msg.DataFid, msg.ControlFid)
	// log.Print("STUB", outputFilename)
	s := app.s.Copy()
	defer s.Close()

	var outID interface{}
	if len(outputFilename) > 0 {
		outID = uploadOutput(s, outputFilename)
	}

	// TODO: store in GRID outputFile archive
	var serOutID string
	if outID != nil {
		ooutID, _ := outID.(bson.ObjectId)
		serOutID = ooutID.Hex()
	}
	if err := db.GetCol(s, "tasks").UpdateId(msg.TaskId, &bson.M{"$set": &bson.M{"output": string(output), "status": model.TASK_STATUS_DONE, "output_fid": serOutID}}); err != nil {
		log.Fatal(err)
	}

	// fmt.Println(output)
	if err != nil {
		fmt.Println("!!!Error:", err)
	}
	if err := delivery.Ack(false); err != nil {
		log.Fatal(err)
	}
}

func (app *contraExecApp) MainLoop() {
	channel, err := app.q.Channel()
	if err != nil { // Add durability with redial action
		log.Fatal(err)
	}
	deliveryCh, err := channel.Consume("execution_queue", app.clientID, false, false, true, false, nil)
	if err != nil {
		log.Fatal(err)
	}
	for {
		delivery := <-deliveryCh
		app.processDelivery(delivery)
	}
}

func main() {
	app := createApp()
	defer app.Stop()
	if len(os.Args) > 1 {
		app.consoleLaunch = true
		dataFid := os.Args[1]
		controlFid := os.Args[2]
		fmt.Println(app.execute(dataFid, controlFid))
	} else {
		app.MainLoop()
	}
}
