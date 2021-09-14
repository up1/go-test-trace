// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// gotest is a tiny program that shells out to `go test`
// and prints the output in color.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

type spanData struct {
	span      oteltrace.Span
	startTime time.Time
}

var danglingSpans = make(map[string]*spanData, 1000)

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:55680", "OpenTelemetry gRPC endpoint to send traces")
	stdin := flag.Bool("stdin", false, "read from stdin")
	flag.Parse()

	ctx := context.Background()
	traceExporter, err := otlptracegrpc.New(
		ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(*endpoint),
		otlptracegrpc.WithDialOption(grpc.WithBlock()),
	)
	if err != nil {
		log.Fatal(err)
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", "go test")))
	if err != nil {
		log.Fatal(err)
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(traceExporter)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)

	t := otel.Tracer("go-test-tracer")
	globalCtx, _ := t.Start(ctx, "go-test-trace")

	defer func() {
		oteltrace.SpanFromContext(globalCtx).End()
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			log.Printf("Failed shutting down the tracer provider: %v", err)
		}
	}()

	if *stdin {
		p, err := newParser(globalCtx, t)
		if err != nil {
			log.Fatal(err)
		}
		if err := p.parse(os.Stdin); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Otherwise, act like a drop-in replacement for `go test`.
	args := append([]string{"test"}, flag.Args()...)
	args = append(args, "-json")
	cmd := exec.Command("go", args...)

	r, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	decoder := json.NewDecoder(r)
	go func() {
		for {
			var data goTestJSON
			if err := decoder.Decode(&data); err != nil {
				log.Fatal(err)
			}
			switch data.Action {
			case "run":
				var span oteltrace.Span
				_, span = t.Start(globalCtx, data.Test, oteltrace.WithTimestamp(data.Time))
				danglingSpans[data.Test] = &spanData{
					span:      span,
					startTime: data.Time,
				}
			case "pass", "fail", "skip":
				span, ok := danglingSpans[data.Test]
				if !ok {
					return // should never happen
				}
				span.span.End(oteltrace.WithTimestamp(data.Time))
			}
			fmt.Print(data.Output)
		}
	}()

	err = cmd.Run()
	oteltrace.SpanFromContext(globalCtx).End()
	if err := tracerProvider.Shutdown(context.Background()); err != nil {
		log.Printf("Failed shutting down the tracer provider: %v", err)
	}
	if err != nil {
		os.Exit(1)
	}
}

type goTestJSON struct {
	Time   time.Time
	Action string
	Test   string
	Output string
}
