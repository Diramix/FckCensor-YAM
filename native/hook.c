typedef __SIZE_TYPE__ size_t;
typedef __UINTPTR_TYPE__ uintptr_t;
typedef __PTRDIFF_TYPE__ ssize_t;
typedef void *(*parse_full_fn)(const char *, void *, int, void **);
extern void *dlopen(const char *, int);
extern void *dlsym(void *, const char *);
extern int mprotect(void *, size_t, int);
extern int open(const char *, int, ...);
extern int close(int);
extern ssize_t read(int, void *, size_t);
extern ssize_t write(int, const void *, size_t);
extern int socket(int, int, int);
extern int connect(int, const void *, unsigned int);
extern int setsockopt(int, int, int, const void *, unsigned int);
extern size_t strlen(const char *);
extern int strcmp(const char *, const char *);
extern int strncmp(const char *, const char *, size_t);
extern char *strstr(const char *, const char *);
extern void *malloc(size_t);
extern void free(void *);
extern void *memcpy(void *, const void *, size_t);
extern void *memset(void *, int, size_t);
extern int getpid(void);
extern int prctl(int, ...);
extern void g_error_free(void *);
extern int access(const char *, int);
extern int snprintf(char *, size_t, const char *, ...);

static parse_full_fn parse_full;
static void *library_handle;
static volatile int active;
static unsigned char saved_code[8];
static unsigned char *entry;

static void log_line(const char *message) {
    int fd = open("/tmp/fckcensor/hook.log", 0x441, 0600);
    if (fd >= 0) {
        write(fd, message, strlen(message));
        close(fd);
    }
}

static void flush_code(void *begin, void *end) {
#if defined(__arm__) && !defined(STATION_TEST)
    register unsigned long r0 __asm__("r0") = (unsigned long)begin;
    register unsigned long r1 __asm__("r1") = (unsigned long)end;
    register unsigned long r2 __asm__("r2") = 0;
    register unsigned long r7 __asm__("r7") = 0xf0002;
    __asm__ volatile("svc 0" : "+r"(r0) : "r"(r1), "r"(r2), "r"(r7) : "memory");
#else
    (void)begin;
    (void)end;
#endif
}

int station_track_id(const char *pipeline, char *id, size_t cap) {
    const char *url = strstr(pipeline, "/music-v2/");
    if (!url) {
        const char *suffix = strstr(pipeline, ".audiostoragecache");
        if (!suffix) return 0;
        const char *first = suffix;
        while (first > pipeline && first[-1] >= '0' && first[-1] <= '9') first--;
        size_t n = suffix - first;
        if (!n || n >= cap || first == pipeline || first[-1] != '/') return 0;
        memcpy(id, first, n); id[n] = 0; return 1;
    }
    const char *end = url;
    while (*end && *end != '\'' && *end != '"' && *end != ' ' && *end != '?' && *end != '!') end++;
    const char *format = end;
    while (format > url && format[-1] != '/') format--;
    if (format <= url || strncmp(format, "aac", 3) && strncmp(format, "mp3", 3) && strncmp(format, "opus", 4) && strncmp(format, "flac", 4)) return 0;
    const char *last = format - 1;
    const char *first = last;
    while (first > url && first[-1] >= '0' && first[-1] <= '9') first--;
    size_t n = last - first;
    if (!n || n >= cap || first <= url || first[-1] != '.') return 0;
    memcpy(id, first, n);
    id[n] = 0;
    return 1;
}

int station_id_from_log(const char *log, const char *thread, char *id, size_t cap) {
    const char *mark = 0;
    if (!strncmp(thread, "AudioPlr_", 9) || !strncmp(thread, "AudioCln_", 9)) mark = thread + 9;
    else if (!strncmp(thread, "gap2_", 5)) mark = thread + 5;
    if (!mark || !*mark) return 0;
    const char *cursor = log;
    while ((cursor = strstr(cursor, "[audio "))) {
        const char *end = strstr(cursor, "]");
        if (!end) break;
        const char *stream = strstr(cursor, "stream=");
        const char *tag = strstr(cursor, "mark=");
        if (stream && tag && stream < end && tag < end &&
            !strncmp(tag + 5, mark, strlen(mark)) && tag + 5 + strlen(mark) == end) {
            stream += 7;
            const char *last = stream;
            while (last < end && *last >= '0' && *last <= '9') last++;
            size_t n = last - stream;
            if (n && n < cap) { memcpy(id, stream, n); id[n] = 0; return 1; }
        }
        cursor = end + 1;
    }
    return 0;
}

static int thread_track_id(char *id, size_t cap) {
    char name[16] = {0};
    if (prctl(16, name, 0, 0, 0)) return 0;
    if (strncmp(name, "AudioPlr_", 9) && strncmp(name, "AudioCln_", 9) && strncmp(name, "gap2_", 5)) return 0;
    int fd = open("/data/quasar/daemons_logs/mediad.log", 0);
    if (fd < 0) return 0;
    char *data = malloc(65537);
    if (!data) { close(fd); return 0; }
    int n = read(fd, data, 65536);
    close(fd);
    int result = 0;
    if (n > 0) { data[n] = 0; result = station_id_from_log(data, name, id, cap); }
    free(data);
    return result;
}

static int resolve(const char *id) {
    if (access("/tmp/fckcensor/enabled", 0) != 0) return 0;
    int fd = socket(1, 1 | 0x80000, 0);
    if (fd < 0) return 0;
    struct { long sec; long usec; } timeout = { 35, 0 };
    setsockopt(fd, 1, 20, &timeout, sizeof(timeout));
    setsockopt(fd, 1, 21, &timeout, sizeof(timeout));
    struct { unsigned short family; char path[108]; } address;
    memset(&address, 0, sizeof(address));
    address.family = 1;
    const char *path = "/tmp/fckcensor/control.sock";
    memcpy(address.path, path, strlen(path) + 1);
    int result = 0;
    if (!connect(fd, &address, sizeof(address))) {
        char request[48];
        int n = snprintf(request, sizeof(request), "%s\n", id);
        if (write(fd, request, n) == n) {
            char response[16];
            int used = 0;
            while (used < 15) {
                int nread = read(fd, response + used, 15 - used);
                if (nread <= 0) break;
                used += nread;
                if (response[used - 1] == '\n') break;
            }
            response[used] = 0;
            result = !strcmp(response, "OK\n");
        }
    }
    close(fd);
    return result;
}

static int append(char *out, size_t cap, size_t *used, const char *s, size_t n) {
    if (*used + n >= cap) return 0;
    memcpy(out + *used, s, n);
    *used += n;
    out[*used] = 0;
    return 1;
}

char *station_rewrite(const char *pipeline, const char *id) {
    size_t cap = strlen(pipeline) + 256;
    char *out = malloc(cap);
    if (!out) return 0;
    size_t used = 0;
    int changed = 0;
    const char *part = pipeline;
    while (*part) {
        const char *end = part;
        char quote = 0;
        while (*end) {
            if ((*end == '\'' || *end == '"') && (end == part || end[-1] != '\\')) {
                if (!quote) quote = *end;
                else if (quote == *end) quote = 0;
            }
            if (*end == '!' && !quote) break;
            end++;
        }
        const char *start = part;
        while (start < end && *start == ' ') start++;
        int decrypt = !strncmp(start, "aesctrdec", 9) || !strncmp(start, "aesdec", 6);
        if (!decrypt) {
            if (used && !append(out, cap, &used, " ! ", 3)) goto fail;
            if (!strncmp(start, "souphttpsrc", 11) || !strncmp(start, "filesrc", 7)) {
                const char *location = strstr(start, "location=");
                if (!location || location >= end) goto fail;
                const char *value = location + 9;
                const char *tail = value;
                if (*tail == '\'' || *tail == '"') {
                    char q = *tail++;
                    while (tail < end && (*tail != q || tail[-1] == '\\')) tail++;
                    if (tail == end) goto fail;
                    tail++;
                } else {
                    while (tail < end && *tail != ' ') tail++;
                }
                char replacement[128];
                int n = snprintf(replacement, sizeof(replacement), "location=\"http://127.0.0.1:18777/audio/%s\"", id);
                if (n < 0 || n >= sizeof(replacement)) goto fail;
                if (!strncmp(start, "filesrc", 7)) {
                    if (!append(out, cap, &used, part, start - part) || !append(out, cap, &used, "souphttpsrc", 11) || !append(out, cap, &used, start + 7, location - start - 7)) goto fail;
                } else if (!append(out, cap, &used, part, location - part)) goto fail;
                if (!append(out, cap, &used, replacement, n) || !append(out, cap, &used, tail, end - tail)) goto fail;
                changed++;
            } else if (!append(out, cap, &used, part, end - part)) goto fail;
        }
        part = *end ? end + 1 : end;
    }
    if (changed == 1) return out;
fail:
    free(out);
    return 0;
}

static void *hook_launch(const char *description, void **error) {
    if (!parse_full) return 0;
    char id[32];
    if (!active || !description) return parse_full(description, 0, 0, error);
    if (!station_track_id(description, id, sizeof(id)) && !thread_track_id(id, sizeof(id))) {
        if (strstr(description, "decodebin")) {
            char name[16] = {0}, message[128];
            prctl(16, name, 0, 0, 0);
            const char *source = strstr(description, "souphttpsrc") ? "souphttpsrc" : strstr(description, "appsrc") ? "appsrc" : strstr(description, "filesrc") ? "filesrc" : "other";
            snprintf(message, sizeof(message), "kept original: track ID unavailable (thread=%s, source=%s)\n", name, source);
            log_line(message);
        }
        return parse_full(description, 0, 0, error);
    }
    if (!resolve(id)) {
        char message[96];
        snprintf(message, sizeof(message), "kept original track %s: replacement unavailable; see service log\n", id);
        log_line(message);
        return parse_full(description, 0, 0, error);
    }
    char *replacement = station_rewrite(description, id);
    if (!replacement) {
        char message[96];
        const char *first = description;
        while (*first == ' ') first++;
        char source[32]; size_t used = 0;
        while (used < sizeof(source) - 1 && first[used] && first[used] != ' ' && first[used] != '!') { source[used] = first[used]; used++; }
        source[used] = 0;
        snprintf(message, sizeof(message), "kept original track %s: unsupported source %s\n", id, source);
        log_line(message);
        return parse_full(description, 0, 0, error);
    }
    void *replacement_error = 0;
    void *result = parse_full(replacement, 0, 0, &replacement_error);
    free(replacement);
    if (replacement_error) g_error_free(replacement_error);
    if (!result) return parse_full(description, 0, 0, error);
    if (result) {
        char message[80];
        snprintf(message, sizeof(message), "replaced track %s\n", id);
        log_line(message);
    }
    return result;
}

int fckcensor_status(void) { return active; }

#ifndef STATION_TEST
__attribute__((constructor)) static void initialize(void) {
    library_handle = dlopen("/system/vendor/quasar/libs/libquasar_gstreamer.so", 2);
    if (!library_handle) return;
    void *symbol = dlsym(library_handle, "gst_parse_launch");
    parse_full = (parse_full_fn)dlsym(library_handle, "gst_parse_launch_full");
    if (!symbol || !((uintptr_t)symbol & 1)) {
        log_line("unsupported GStreamer ABI\n");
        return;
    }
    entry = (unsigned char *)((uintptr_t)symbol & ~1U);
    if ((uintptr_t)entry & 3) return;
    uintptr_t page = (uintptr_t)entry & ~4095U;
    if (mprotect((void *)page, 8192, 7)) return;
    const unsigned char expected[6] = { 0x0b, 0x46, 0x00, 0x21, 0x00, 0x22 };
    for (int i = 0; i < 6; i++) if (entry[i] != expected[i]) {
        mprotect((void *)page, 8192, 5);
        log_line("unsupported gst_parse_launch implementation\n");
        return;
    }
    if (!parse_full) {
        unsigned short first = *(unsigned short *)(entry + 6);
        unsigned short second = *(unsigned short *)(entry + 8);
        if ((first & 0xf800) != 0xf000 || (second & 0xd000) != 0x9000) {
            mprotect((void *)page, 8192, 5);
            return;
        }
        unsigned int sign = (first >> 10) & 1;
        unsigned int i1 = (~((second >> 13) ^ sign)) & 1;
        unsigned int i2 = (~((second >> 11) ^ sign)) & 1;
        int displacement = (sign << 24) | (i1 << 23) | (i2 << 22) | ((first & 0x3ff) << 12) | ((second & 0x7ff) << 1);
        if (sign) displacement |= 0xfe000000;
        parse_full = (parse_full_fn)(((uintptr_t)(entry + 10) + displacement) | 1);
    }
    memcpy(saved_code, entry, 8);
    unsigned int patch[2] = { 0xf000f8df, (uintptr_t)hook_launch };
    memcpy(entry, patch, 8);
    flush_code(entry, entry + 8);
    mprotect((void *)page, 8192, 5);
    active = 1;
    char path[96];
    snprintf(path, sizeof(path), "/tmp/fckcensor/installed-%d", getpid());
    int fd = open(path, 0x241, 0600);
    if (fd >= 0) close(fd);
    log_line("hook installed\n");
}

#endif
