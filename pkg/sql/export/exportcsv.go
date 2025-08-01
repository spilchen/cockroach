// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package export

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/encoding/csv"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/unique"
	"github.com/cockroachdb/errors"
)

const (
	exportFilePatternPart    = "%part%"
	exportFilePatternDefault = exportFilePatternPart + ".csv"
)

// csvExporter data structure to augment the compression
// and csv writer, encapsulating the internals to make
// exporting oblivious for the consumers.
type csvExporter struct {
	compressor *gzip.Writer
	buf        *bytes.Buffer
	csvWriter  *csv.Writer
}

// Write append record to csv file.
func (c *csvExporter) Write(record []string) error {
	return c.csvWriter.Write(record)
}

// Close closes the compressor writer which
// appends archive footers.
func (c *csvExporter) Close() error {
	if c.compressor != nil {
		return c.compressor.Close()
	}
	return nil
}

// Flush flushes both csv and compressor writer if
// initialized.
func (c *csvExporter) Flush() error {
	c.csvWriter.Flush()
	if c.compressor != nil {
		return c.compressor.Flush()
	}
	return nil
}

// ResetBuffer resets the buffer and compressor state.
func (c *csvExporter) ResetBuffer() {
	c.buf.Reset()
	if c.compressor != nil {
		// Brings compressor to its initial state.
		c.compressor.Reset(c.buf)
	}
}

// Bytes results in the slice of bytes with compressed content.
func (c *csvExporter) Bytes() []byte {
	return c.buf.Bytes()
}

// Len returns length of the buffer with content.
func (c *csvExporter) Len() int {
	return c.buf.Len()
}

func (c *csvExporter) FileName(spec execinfrapb.ExportSpec, part string) string {
	pattern := exportFilePatternDefault
	if spec.NamePattern != "" {
		pattern = spec.NamePattern
	}

	fileName := strings.Replace(pattern, exportFilePatternPart, part, -1)
	// TODO: add suffix based on compressor type
	if c.compressor != nil {
		fileName += ".gz"
	}
	return fileName
}

func newCSVExporter(sp execinfrapb.ExportSpec) *csvExporter {
	buf := bytes.NewBuffer([]byte{})
	var exporter *csvExporter
	switch sp.Format.Compression {
	case roachpb.IOFileFormat_Gzip:
		{
			writer := gzip.NewWriter(buf)
			exporter = &csvExporter{
				compressor: writer,
				buf:        buf,
				csvWriter:  csv.NewWriter(writer),
			}
		}
	default:
		{
			exporter = &csvExporter{
				buf:       buf,
				csvWriter: csv.NewWriter(buf),
			}
		}
	}
	if sp.Format.Csv.Comma != 0 {
		exporter.csvWriter.Comma = sp.Format.Csv.Comma
	}
	return exporter
}

func NewCSVWriterProcessor(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	spec execinfrapb.ExportSpec,
	post *execinfrapb.PostProcessSpec,
	input execinfra.RowSource,
) (execinfra.Processor, error) {
	c := &csvWriter{
		flowCtx:     flowCtx,
		processorID: processorID,
		spec:        spec,
		input:       input,
	}
	semaCtx := tree.MakeSemaContext(nil /* resolver */)
	if err := c.out.Init(ctx, post, colinfo.ExportColumnTypes, &semaCtx, flowCtx.EvalCtx, flowCtx); err != nil {
		return nil, err
	}
	return c, nil
}

type csvWriter struct {
	flowCtx     *execinfra.FlowCtx
	processorID int32
	spec        execinfrapb.ExportSpec
	input       execinfra.RowSource
	out         execinfra.ProcOutputHelper
}

var _ execinfra.Processor = &csvWriter{}

func (sp *csvWriter) OutputTypes() []*types.T {
	return sp.out.OutputTypes
}

func (sp *csvWriter) MustBeStreaming() bool {
	return false
}

func (sp *csvWriter) Run(ctx context.Context, output execinfra.RowReceiver) {
	ctx, span := tracing.ChildSpan(ctx, "csvWriter")
	defer span.Finish()

	instanceID := sp.flowCtx.EvalCtx.NodeID.SQLInstanceID()
	uniqueID := unique.GenerateUniqueInt(unique.ProcessUniqueID(instanceID))

	err := func() error {
		typs := sp.input.OutputTypes()
		sp.input.Start(ctx)
		input := execinfra.MakeNoMetadataRowSource(sp.input, output)

		alloc := &tree.DatumAlloc{}

		writer := newCSVExporter(sp.spec)
		if sp.spec.HeaderRow {
			if err := writer.Write(sp.spec.ColNames); err != nil {
				return err
			}
		}

		var nullsAs string
		if sp.spec.Format.Csv.NullEncoding != nil {
			nullsAs = *sp.spec.Format.Csv.NullEncoding
		}
		f := tree.NewFmtCtx(tree.FmtExport)
		defer f.Close()

		csvRow := make([]string, len(typs))

		chunk := 0
		done := false
		for {
			var rows int64
			writer.ResetBuffer()
			for {
				// If the bytes.Buffer sink exceeds the target size of a CSV file, we
				// flush before exporting any additional rows.
				if int64(writer.buf.Len()) >= sp.spec.ChunkSize {
					break
				}
				if sp.spec.ChunkRows > 0 && rows >= sp.spec.ChunkRows {
					break
				}
				row, err := input.NextRow()
				if err != nil {
					return err
				}
				if row == nil {
					done = true
					break
				}
				rows++

				for i, ed := range row {
					if ed.IsNull() {
						if sp.spec.Format.Csv.NullEncoding != nil {
							csvRow[i] = nullsAs
							continue
						} else {
							return errors.New("NULL value encountered during EXPORT, " +
								"use `WITH nullas` to specify the string representation of NULL")
						}
					}
					if err := ed.EnsureDecoded(typs[i], alloc); err != nil {
						return err
					}
					ed.Datum.Format(f)
					csvRow[i] = f.String()
					f.Reset()
				}
				if err := writer.Write(csvRow); err != nil {
					return err
				}
			}
			if rows < 1 {
				break
			}
			if err := writer.Flush(); err != nil {
				return errors.Wrap(err, "failed to flush csv writer")
			}

			res, err := func() (rowenc.EncDatumRow, error) {
				conf, err := cloud.ExternalStorageConfFromURI(sp.spec.Destination, sp.spec.User())
				if err != nil {
					return nil, err
				}
				es, err := sp.flowCtx.Cfg.ExternalStorage(ctx, conf)
				if err != nil {
					return nil, err
				}
				defer es.Close()

				part := fmt.Sprintf("n%d.%d", uniqueID, chunk)
				chunk++
				filename := writer.FileName(sp.spec, part)
				// Close writer to ensure buffer and any compression footer is flushed.
				err = writer.Close()
				if err != nil {
					return nil, errors.Wrapf(err, "failed to close exporting writer")
				}

				size := writer.Len()

				if err := cloud.WriteFile(ctx, es, filename, bytes.NewReader(writer.Bytes())); err != nil {
					return nil, err
				}
				return rowenc.EncDatumRow{
					rowenc.DatumToEncDatum(types.String, tree.NewDString(filename)),
					rowenc.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(rows))),
					rowenc.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(size))),
				}, nil
			}()
			if err != nil {
				return err
			}

			cs, err := sp.out.EmitRow(ctx, res, output)
			if err != nil {
				return err
			}
			if cs != execinfra.NeedMoreRows {
				// We don't return an error here because we want the error (if any) that
				// actually caused the consumer to enter a closed/draining state to take precendence.
				return nil
			}
			if done {
				break
			}
		}

		return nil
	}()

	execinfra.DrainAndClose(ctx, sp.flowCtx, sp.input, output, err)
}

// Resume is part of the execinfra.Processor interface.
func (sp *csvWriter) Resume(output execinfra.RowReceiver) {
	panic("not implemented")
}

// Close is part of the execinfra.Processor interface.
func (*csvWriter) Close(context.Context) {}
