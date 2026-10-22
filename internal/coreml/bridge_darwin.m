//go:build darwin

#import <CoreML/CoreML.h>
#import <Foundation/Foundation.h>

#include <stdlib.h>
#include <string.h>

#include "bridge.h"

struct cm_model {
    void *model; // MLModel, retained across the C boundary
};

static char *copy_string(NSString *s) {
    return strdup(s ? s.UTF8String : "unknown error");
}

static char *error_string(NSError *e) {
    if (e == nil) {
        return strdup("CoreML reported no error");
    }
    return copy_string([NSString stringWithFormat:@"%@ (%@ %ld)", e.localizedDescription, e.domain, (long)e.code]);
}

cm_model *cm_load(const char *path, const char *function, int units, char **err) {
    @autoreleasepool {
        @try {
            NSURL *url = [NSURL fileURLWithPath:[NSString stringWithUTF8String:path]];
            MLModelConfiguration *cfg = [[MLModelConfiguration alloc] init];
            cfg.computeUnits = (MLComputeUnits)units;
            if (function != NULL && function[0] != '\0') {
                if (@available(macOS 15.0, *)) {
                    cfg.functionName = [NSString stringWithUTF8String:function];
                } else {
                    *err = strdup("models with several functions need macOS 15 or later");
                    return NULL;
                }
            }
            NSError *e = nil;
            MLModel *m = [MLModel modelWithContentsOfURL:url configuration:cfg error:&e];
            if (m == nil) {
                *err = error_string(e);
                return NULL;
            }
            cm_model *h = calloc(1, sizeof *h);
            h->model = (void *)CFBridgingRetain(m);
            return h;
        } @catch (NSException *x) {
            *err = copy_string(x.reason);
            return NULL;
        }
    }
}

static NSString *type_name(MLMultiArrayDataType t) {
    switch (t) {
    case MLMultiArrayDataTypeFloat16:
        return @"float16";
    case MLMultiArrayDataTypeFloat32:
        return @"float32";
    case MLMultiArrayDataTypeDouble:
        return @"float64";
    case MLMultiArrayDataTypeInt32:
        return @"int32";
    default:
        return @"other";
    }
}

static NSArray *describe(NSDictionary<NSString *, MLFeatureDescription *> *features) {
    NSMutableArray *out = [NSMutableArray array];
    for (NSString *name in [features.allKeys sortedArrayUsingSelector:@selector(compare:)]) {
        MLFeatureDescription *f = features[name];
        MLMultiArrayConstraint *c = f.multiArrayConstraint;
        if (c == nil) {
            [out addObject:@{@"name" : name, @"type" : @"other", @"shape" : @[]}];
            continue;
        }
        [out addObject:@{@"name" : name, @"type" : type_name(c.dataType), @"shape" : c.shape}];
    }
    return out;
}

char *cm_describe(cm_model *h, char **err) {
    @autoreleasepool {
        MLModel *m = (__bridge MLModel *)h->model;
        NSDictionary *d = @{
            @"inputs" : describe(m.modelDescription.inputDescriptionsByName),
            @"outputs" : describe(m.modelDescription.outputDescriptionsByName),
        };
        NSError *e = nil;
        NSData *json = [NSJSONSerialization dataWithJSONObject:d options:0 error:&e];
        if (json == nil) {
            *err = error_string(e);
            return NULL;
        }
        char *s = malloc(json.length + 1);
        memcpy(s, json.bytes, json.length);
        s[json.length] = '\0';
        return s;
    }
}

// input wraps one float32 buffer as the multi-array the model declares. A
// float32 input is wrapped in place; any other type is converted into a buffer
// the array owns and frees.
static MLMultiArray *input(MLMultiArrayConstraint *c, float *data, int64_t count, NSString *name, char **err) {
    NSArray<NSNumber *> *shape = c.shape;
    int64_t want = 1;
    for (NSNumber *n in shape) {
        want *= n.longLongValue;
    }
    if (want != count) {
        *err = copy_string([NSString stringWithFormat:@"input %@ wants %lld values, got %lld", name, want, count]);
        return nil;
    }
    NSMutableArray<NSNumber *> *strides = [NSMutableArray arrayWithCapacity:shape.count];
    int64_t s = 1;
    for (NSInteger i = (NSInteger)shape.count - 1; i >= 0; i--) {
        [strides insertObject:@(s) atIndex:0];
        s *= shape[i].longLongValue;
    }

    void *buf = data;
    void (^dealloc)(void *) = nil;
    switch (c.dataType) {
    case MLMultiArrayDataTypeFloat32:
        break;
    case MLMultiArrayDataTypeFloat16: {
        _Float16 *h = malloc(sizeof(_Float16) * (size_t)count);
        for (int64_t i = 0; i < count; i++) {
            h[i] = (_Float16)data[i];
        }
        buf = h;
        dealloc = ^(void *p) { free(p); };
        break;
    }
    case MLMultiArrayDataTypeInt32: {
        int32_t *v = malloc(sizeof(int32_t) * (size_t)count);
        for (int64_t i = 0; i < count; i++) {
            v[i] = (int32_t)data[i];
        }
        buf = v;
        dealloc = ^(void *p) { free(p); };
        break;
    }
    default:
        *err = copy_string([NSString stringWithFormat:@"input %@ has an element type this bridge does not convert", name]);
        return nil;
    }
    NSError *e = nil;
    MLMultiArray *a = [[MLMultiArray alloc] initWithDataPointer:buf
                                                          shape:shape
                                                       dataType:c.dataType
                                                        strides:strides
                                                    deallocator:dealloc
                                                          error:&e];
    if (a == nil) {
        if (dealloc != nil) {
            dealloc(buf);
        }
        *err = error_string(e);
    }
    return a;
}

// output copies a result array into a dense float32 buffer. Arrays the Neural
// Engine hands back are often padded along their last axis, so this walks the
// array's own strides rather than assuming it is dense.
static int output(MLMultiArray *a, float *dst, int64_t count, NSString *name, char **err) {
    if (a.count != count) {
        *err = copy_string([NSString stringWithFormat:@"output %@ has %ld values, the caller expects %lld",
                                                      name, (long)a.count, count]);
        return -1;
    }
    NSInteger rank = (NSInteger)a.shape.count;
    if (rank == 0 || rank > 8) {
        *err = copy_string([NSString stringWithFormat:@"output %@ has rank %ld", name, (long)rank]);
        return -1;
    }
    int64_t shape[8], strides[8];
    for (NSInteger i = 0; i < rank; i++) {
        shape[i] = a.shape[i].longLongValue;
        strides[i] = a.strides[i].longLongValue;
    }
    MLMultiArrayDataType type = a.dataType;
    // A block cannot capture an array, only a pointer to one.
    const int64_t *shp = shape, *str = strides;
    __block int rc = 0;
    [a getBytesWithHandler:^(const void *bytes, NSInteger size) {
        (void)size;
        int64_t inner = shp[rank - 1], step = str[rank - 1];
        int64_t rows = count / inner;
        int64_t idx[8] = {0};
        for (int64_t r = 0; r < rows; r++) {
            int64_t off = 0;
            for (NSInteger d = 0; d < rank - 1; d++) {
                off += idx[d] * str[d];
            }
            float *o = dst + r * inner;
            switch (type) {
            case MLMultiArrayDataTypeFloat16: {
                const _Float16 *s = (const _Float16 *)bytes + off;
                for (int64_t k = 0; k < inner; k++) {
                    o[k] = (float)s[k * step];
                }
                break;
            }
            case MLMultiArrayDataTypeFloat32: {
                const float *s = (const float *)bytes + off;
                for (int64_t k = 0; k < inner; k++) {
                    o[k] = s[k * step];
                }
                break;
            }
            case MLMultiArrayDataTypeInt32: {
                const int32_t *s = (const int32_t *)bytes + off;
                for (int64_t k = 0; k < inner; k++) {
                    o[k] = (float)s[k * step];
                }
                break;
            }
            case MLMultiArrayDataTypeDouble: {
                const double *s = (const double *)bytes + off;
                for (int64_t k = 0; k < inner; k++) {
                    o[k] = (float)s[k * step];
                }
                break;
            }
            default:
                rc = -1;
                return;
            }
            for (NSInteger d = rank - 2; d >= 0; d--) {
                if (++idx[d] < shp[d]) {
                    break;
                }
                idx[d] = 0;
            }
        }
    }];
    if (rc != 0) {
        *err = copy_string([NSString stringWithFormat:@"output %@ has an element type this bridge does not convert", name]);
    }
    return rc;
}

int cm_predict(cm_model *h,
               int n_in, const char **in_names, float **in_data, const int64_t *in_counts,
               int n_out, const char **out_names, float **out_data, const int64_t *out_counts,
               char **err) {
    @autoreleasepool {
        @try {
            MLModel *m = (__bridge MLModel *)h->model;
            NSDictionary<NSString *, MLFeatureDescription *> *descs = m.modelDescription.inputDescriptionsByName;
            NSMutableDictionary<NSString *, MLFeatureValue *> *feed = [NSMutableDictionary dictionaryWithCapacity:n_in];
            for (int i = 0; i < n_in; i++) {
                NSString *name = [NSString stringWithUTF8String:in_names[i]];
                MLFeatureDescription *d = descs[name];
                if (d == nil || d.multiArrayConstraint == nil) {
                    *err = copy_string([NSString stringWithFormat:@"the model has no array input %@", name]);
                    return -1;
                }
                MLMultiArray *a = input(d.multiArrayConstraint, in_data[i], in_counts[i], name, err);
                if (a == nil) {
                    return -1;
                }
                feed[name] = [MLFeatureValue featureValueWithMultiArray:a];
            }
            NSError *e = nil;
            MLDictionaryFeatureProvider *p = [[MLDictionaryFeatureProvider alloc] initWithDictionary:feed error:&e];
            if (p == nil) {
                *err = error_string(e);
                return -1;
            }
            id<MLFeatureProvider> res = [m predictionFromFeatures:p error:&e];
            if (res == nil) {
                *err = error_string(e);
                return -1;
            }
            for (int i = 0; i < n_out; i++) {
                NSString *name = [NSString stringWithUTF8String:out_names[i]];
                MLMultiArray *a = [res featureValueForName:name].multiArrayValue;
                if (a == nil) {
                    *err = copy_string([NSString stringWithFormat:@"the model has no array output %@", name]);
                    return -1;
                }
                if (output(a, out_data[i], out_counts[i], name, err) != 0) {
                    return -1;
                }
            }
            return 0;
        } @catch (NSException *x) {
            *err = copy_string(x.reason);
            return -1;
        }
    }
}

void cm_free(cm_model *h) {
    if (h == NULL) {
        return;
    }
    @autoreleasepool {
        if (h->model != NULL) {
            CFBridgingRelease(h->model);
        }
    }
    free(h);
}
