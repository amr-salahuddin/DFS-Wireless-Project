package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	pb "proj/Services"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	masterAddress = "localhost:50061" // Address of the master node
	maxGRPCSize   = 1024 * 1024 * 100 // 100 MB
)

type DataNodeServer struct {
	IP            string
	PortForMaster string `json:"MasterNodePort"`
	PortForClient string `json:"ClientNodePort"`
	PortForDN     string `json:"DataNodePort"`
	ID            int32  `json:"ID"`
	pb.UnimplementedFileServiceServer
	openFiles map[string]*os.File
}

/*
Handles file upload from client
*/
func (d *DataNodeServer) UploadFile(ctx context.Context, req *pb.FileUploadRequest) (*pb.FileUploadResponse, error) {
	log.Printf("Received upload request for: %s", req.FileName)

	// Metadata extraction (client IP and port)
	md, exists := metadata.FromIncomingContext(ctx)
	if !exists {
		log.Println("No metadata in request")
	}
	clientIP := strings.Join(md.Get("client-ip"), ",")
	clientPort := strings.Join(md.Get("client-port"), ",")

	outMeta := metadata.Pairs("client-ip", clientIP, "client-port", clientPort)
	outCtx := metadata.NewOutgoingContext(context.Background(), outMeta)

	// Save directory for this DataNode
	nodeDir := fmt.Sprintf("./uploaded_%s_%s", d.IP, d.PortForClient[1:])
	if err := os.MkdirAll(nodeDir, 0755); err != nil {
		return nil, fmt.Errorf("error creating upload dir: %v", err)
	}

	// File saving path
	savePath := filepath.Join(nodeDir, req.FileName)
	file, err := os.Create(savePath)
	if err != nil {
		return nil, fmt.Errorf("error creating file: %v", err)
	}
	defer file.Close()

	if _, err := file.Write(req.FileContent); err != nil {
		return nil, fmt.Errorf("error writing file content: %v", err)
	}

	log.Printf("File stored at: %s", savePath)

	// Write the content to the file

	log.Printf("File uploaded success at %s", savePath)

	// Asynchronously notify the master node about the upload
	go notifyMasterOfUpload(d, outCtx, req.FileName, savePath)

	return &pb.FileUploadResponse{Message: "Upload successful"}, nil
}

const chunkSize = 1024 * 1024 // 1MB chunk size

func (d *DataNodeServer) Replicate(ctx context.Context, req *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
	log.Printf("Replicating file: %s to %d node(s)", req.FileName, len(req.IpAddresses))

	// Read the file content
	content, err := os.ReadFile(req.FilePath)
	if err != nil {
		return nil, fmt.Errorf("replication failed, cannot read file: %v", err)
	}
	totalSize := len(content)

	// Iterate over the provided IP addresses and ports
	for i, ip := range req.IpAddresses {
		addr := fmt.Sprintf("%s:%d", ip, req.PortNumbers[i])
		conn, err := grpc.Dial(addr, grpc.WithInsecure())
		if err != nil {
			log.Printf("Connection failed to %s: %v", addr, err)
			continue
		}
		client := pb.NewFileServiceClient(conn)

		// STEP 1: Begin Upload
		_, err = client.BeginUploadFile(ctx, &pb.FileUploadRequest{
			FileName: req.FileName,
		})
		if err != nil {
			log.Printf("Replication BeginUpload failed to %s: %v", addr, err)
			conn.Close()
			continue
		}
		log.Printf("Replication started for %s on %s", req.FileName, addr)

		// STEP 2: Update Upload with chunks and progress logging
		var replicateError error
		for offset := 0; offset < totalSize; offset += chunkSize {
			end := offset + chunkSize
			if end > totalSize {
				end = totalSize
			}
			chunk := content[offset:end]
			_, err := client.UpdateUploadFile(ctx, &pb.FileUploadRequest{
				FileName:    req.FileName,
				FileContent: chunk,
			})
			if err != nil {
				log.Printf("Replication UpdateUpload failed to %s at offset %d: %v", addr, offset, err)
				replicateError = err
				break
			}

			progress := float64(end) / float64(totalSize) * 100
			log.Printf("Replication progress to %s: %.2f%%", addr, progress)
		}

		// STEP 3: End Upload (only if no error occurred during chunk updates)
		if replicateError == nil {
			_, err := client.EndUploadFile(ctx, &pb.FileUploadRequest{
				FileName: req.FileName,
			})
			if err != nil {
				log.Printf("Replication EndUpload failed to %s: %v", addr, err)
			} else {
				log.Printf("Replication completed successfully to %s", addr)
			}
		} else {
			log.Printf("Replication to %s encountered an error; skipping EndUpload", addr)
		}

		conn.Close()
	}
	return &pb.ReplicateResponse{}, nil
}

func (d *DataNodeServer) BeginUploadFile(ctx context.Context, req *pb.FileUploadRequest) (*pb.FileUploadResponse, error) {
	log.Printf("Begin upload for: %s", req.FileName)

	// Metadata extraction
	_, exists := metadata.FromIncomingContext(ctx)
	if !exists {
		log.Println("No metadata in request")
	}

	// Save directory
	nodeDir := fmt.Sprintf("./uploaded_%s_%s", d.IP, d.PortForClient[1:])
	if err := os.MkdirAll(nodeDir, 0755); err != nil {
		return nil, fmt.Errorf("error creating upload dir: %v", err)
	}

	savePath := filepath.Join(nodeDir, req.FileName)
	file, err := os.Create(savePath)
	if err != nil {
		return nil, fmt.Errorf("error creating file: %v", err)
	}

	if d.openFiles == nil {
		d.openFiles = make(map[string]*os.File)
	}
	d.openFiles[req.FileName] = file

	log.Printf("File created at: %s", savePath)
	return &pb.FileUploadResponse{Message: "Upload initiated"}, nil
}

func (d *DataNodeServer) UpdateUploadFile(ctx context.Context, req *pb.FileUploadRequest) (*pb.FileUploadResponse, error) {
	file, ok := d.openFiles[req.FileName]
	if !ok {
		return nil, fmt.Errorf("file not found in active uploads: %s", req.FileName)
	}

	if _, err := file.Write(req.FileContent); err != nil {
		return nil, fmt.Errorf("error writing file content: %v", err)
	}

	log.Printf("Chunk written to %s", req.FileName)
	return &pb.FileUploadResponse{Message: "Chunk received"}, nil
}

func (d *DataNodeServer) EndUploadFile(ctx context.Context, req *pb.FileUploadRequest) (*pb.FileUploadResponse, error) {
	file, ok := d.openFiles[req.FileName]
	if !ok {
		return nil, fmt.Errorf("file not found in active uploads: %s", req.FileName)
	}

	file.Close()
	delete(d.openFiles, req.FileName)

	log.Printf("Upload finished for %s", req.FileName)

	// Metadata for notifying master
	md, exists := metadata.FromIncomingContext(ctx)
	if !exists {
		log.Println("No metadata in request")
	}
	clientIP := strings.Join(md.Get("client-ip"), ",")
	clientPort := strings.Join(md.Get("client-port"), ",")
	outMeta := metadata.Pairs("client-ip", clientIP, "client-port", clientPort)
	outCtx := metadata.NewOutgoingContext(context.Background(), outMeta)

	savePath := fmt.Sprintf("./uploaded_%s_%s/%s", d.IP, d.PortForClient[1:], req.FileName)
	go notifyMasterOfUpload(d, outCtx, req.FileName, savePath)

	return &pb.FileUploadResponse{Message: "Upload complete"}, nil
}

func notifyMasterOfUpload(d *DataNodeServer, ctx context.Context, filename, path string) {
	conn, err := grpc.Dial(masterAddress, grpc.WithInsecure())
	if err != nil {
		log.Printf("Failed to notify master: %v", err)
		return
	}
	defer conn.Close()

	client := pb.NewFileServiceClient(conn)

	_, err = client.NotifyUploaded(ctx, &pb.NotifyUploadedRequest{
		FileName: filename,
		DataNode: d.ID,
		FilePath: path,
	})
	if err != nil {
		log.Printf("Master notification failed: %v", err)
	}
}

func (d *DataNodeServer) DownloadFile(ctx context.Context, in *pb.FileDownloadRequest) (*pb.FileDownloadResponse, error) {
	log.Printf("FileDownloadRequest %s", in.FileName)
	dir := fmt.Sprintf("./uploaded_%s_%s", d.IP, d.PortForClient[1:])

	filePath := filepath.Join(dir, in.FileName)

	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("ReadFile fail %v", err)
	}
	// Create and return the response with the file content
	response := &pb.FileDownloadResponse{
		FileContent: fileContent,
	}
	return response, nil
}

func (d *DataNodeServer) BeginDownloadFile(ctx context.Context, in *pb.FileDownloadRequest) (*pb.FileDownloadResponse, error) {
	log.Printf("FileDownloadRequest %s", in.FileName)
	dir := fmt.Sprintf("./uploaded_%s_%s", d.IP, d.PortForClient[1:])

	filePath := filepath.Join(dir, in.FileName)

	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("ReadFile fail %v", err)
	}
	// Create and return the response with the file content
	response := &pb.FileDownloadResponse{
		FileContent: fileContent,
	}
	return response, nil
}

func (d *DataNodeServer) UpdateDownloadFile(ctx context.Context, in *pb.FileDownloadRequest) (*pb.FileDownloadResponse, error) {
	log.Printf("FileDownloadRequest %s", in.FileName)
	dir := fmt.Sprintf("./uploaded_%s_%s", d.IP, d.PortForClient[1:])

	filePath := filepath.Join(dir, in.FileName)

	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("ReadFile fail %v", err)
	}
	// Create and return the response with the file content
	response := &pb.FileDownloadResponse{
		FileContent: fileContent,
	}
	return response, nil
}

func (d *DataNodeServer) EndDownloadFile(ctx context.Context, in *pb.FileDownloadRequest) (*pb.FileDownloadResponse, error) {
	log.Printf("FileDownloadRequest %s", in.FileName)
	dir := fmt.Sprintf("./uploaded_%s_%s", d.IP, d.PortForClient[1:])

	filePath := filepath.Join(dir, in.FileName)

	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("ReadFile fail %v", err)
	}
	// Create and return the response with the file content
	response := &pb.FileDownloadResponse{
		FileContent: fileContent,
	}
	return response, nil
}

func (d *DataNodeServer) sendHeartbeat() {

	masterConn, err := grpc.Dial(masterAddress, grpc.WithInsecure())

	if err != nil {
		log.Fatalf("Cannot connect to Master %v", err)
	}
	defer masterConn.Close()
	masterClient := pb.NewFileServiceClient(masterConn)
	for {

		time.Sleep(time.Second)
		keepAliveRequest := &pb.KeepAliveRequest{
			DataNode_IP: d.IP,
			PortNumber:  []string{d.PortForMaster, d.PortForClient, d.PortForDN},
			IsAlive:     true,
		}

		_, err := masterClient.KeepAlive(context.Background(), keepAliveRequest)
		if err != nil {
			log.Printf("Cannot Send KeepAlive %v", err)
		}
	}
}

//func (d *DataNodeServer) Replicate(ctx context.Context, req *pb.ReplicateRequest) (*pb.ReplicateResponse, error) {
//
//	log.Printf("Replicating file: %s to %d node(s)", req.FileName, len(req.IpAddresses))
//
//	// Read the content of the file
//	content, err := os.ReadFile(req.FilePath)
//	if err != nil {
//		return nil, fmt.Errorf("replication failed, cannot read file: %v", err)
//	}
//
//	// Iterate over the provided IP addresses and ports
//	for i, ip := range req.IpAddresses {
//
//		addr := fmt.Sprintf("%s:%d", ip, req.PortNumbers[i])
//
//		conn, err := grpc.Dial(addr, grpc.WithInsecure())
//		if err != nil {
//			log.Printf("Connection failed to %s: %v", addr, err)
//			continue
//		}
//		defer conn.Close()
//
//		client := pb.NewFileServiceClient(conn)
//
//		// Invoke the UploadFile service
//		_, err = client.UploadFile(ctx, &pb.FileUploadRequest{
//			FileName:    req.FileName,
//			FileContent: content,
//		})
//		if err != nil {
//			log.Printf("Replication upload failed to %s: %v", addr, err)
//		} else {
//			log.Printf("Replicated to %s", addr)
//		}
//
//	}
//
//	return &pb.ReplicateResponse{}, nil
//}

/*
This function extracts the local IP that the data node runs on
*/
func GetMachineIP() (string, error) {
	// get the network interfaces within machine (ethernet0, wifi ..)
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		// Skip down or loopback interfaces
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP

			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue // not an IPv4 address
			}

			// Found a valid IPv4 on an active, non-loopback interface
			return ip.String(), nil
		}
	}

	return "", fmt.Errorf("no suitable IP address found")
}

func main() {
	// the config file must be passed
	if len(os.Args) < 2 {
		log.Fatalf("Please pass the dataNode configuration file by terminal")
	}

	config_file_path := os.Args[1]
	config, err := os.ReadFile(config_file_path)
	if err != nil {
		log.Fatalf("couldn't read the file specified")
	}

	ip, err := GetMachineIP()
	if err != nil {
		fmt.Println("Error in extracting IP of machine", err)
	}
	// Start to configure our data node server
	dataServer := &DataNodeServer{
		IP: ip,
	}
	// parse the json configuration to the data Node server
	err = json.Unmarshal(config, dataServer)
	if err != nil {
		log.Fatalf("couldn't parse config file")
	}

	// open TCP ports for future connections with Master, Client, DataNodes
	lisC, err := net.Listen("tcp", dataServer.PortForClient)
	if err != nil {
		log.Fatalf("tcp portForClient listen fail %v", err)
	}
	lisD, err := net.Listen("tcp", dataServer.PortForDN)
	if err != nil {
		log.Fatalf("tcp portForDN listen fail %v", err)
	}

	lisMaster, err := net.Listen("tcp", dataServer.PortForMaster)
	if err != nil {
		log.Fatalf("tcp portForM listen fail %v", err)
	}

	// create a Grpc server and bind our data node server to it
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(maxGRPCSize))
	pb.RegisterFileServiceServer(grpcServer, dataServer)

	// Start serving each listener in separate goroutines
	go grpcServer.Serve(lisC)      // Serve on client port
	go grpcServer.Serve(lisD)      // Serve on DataNode port
	go grpcServer.Serve(lisMaster) // Serve on master port
	// tell the master I'm online
	go dataServer.sendHeartbeat()

	log.Printf("DataNode running at %s for client and %s for DataNodes and %s for Master", lisC.Addr(), lisD.Addr(), lisMaster.Addr())
	// blocker so that the code doesn't terminate
	select {}
}
