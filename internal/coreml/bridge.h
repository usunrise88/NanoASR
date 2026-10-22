// The C face of bridge_darwin.m: what the Go side of the package calls.
//
// Every function that can fail takes a char** and, on failure, stores a
// malloc'd message there for the caller to free.

#ifndef NANOASR_COREML_BRIDGE_H
#define NANOASR_COREML_BRIDGE_H

#include <stdint.h>

typedef struct cm_model cm_model;

// cm_load loads a compiled model (.mlmodelc). function names one function of
// a multifunction model, or is empty for the default one. units is an
// MLComputeUnits value.
cm_model *cm_load(const char *path, const char *function, int units, char **err);

// cm_describe returns the model's inputs and outputs as a malloc'd JSON
// object: {"inputs":[{"name","type","shape"}],"outputs":[...]}.
char *cm_describe(cm_model *m, char **err);

// cm_predict runs the model once. Inputs and outputs are float32 buffers of
// exactly the element count their feature declares; the bridge converts to
// and from the model's own element type and strides. Returns 0 on success.
int cm_predict(cm_model *m,
               int n_in, const char **in_names, float **in_data, const int64_t *in_counts,
               int n_out, const char **out_names, float **out_data, const int64_t *out_counts,
               char **err);

void cm_free(cm_model *m);

#endif
