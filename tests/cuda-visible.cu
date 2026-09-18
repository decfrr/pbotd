#include <cuda_runtime.h>
#include <cstdio>
#include <cstdlib>
#include <string>

static void check(cudaError_t error) {
    if (error != cudaSuccess) {
        std::fprintf(stderr, "%s\n", cudaGetErrorString(error));
        std::exit(1);
    }
}

// Compare CUDA's actual device UUIDs with the scheduler's allocation order.
int main() {
    const char* allocation = std::getenv("CUDA_VISIBLE_DEVICES");
    if (!allocation || !*allocation) return 2;
    int count = 0;
    check(cudaGetDeviceCount(&count));
    std::string visible;
    for (int device = 0; device < count; ++device) {
        cudaDeviceProp properties{};
        check(cudaGetDeviceProperties(&properties, device));
        if (device) visible += ',';
        visible += "GPU-";
        for (int i = 0; i < 16; ++i) {
            if (i == 4 || i == 6 || i == 8 || i == 10) visible += '-';
            char byte[3];
            std::snprintf(byte, sizeof(byte), "%02x", static_cast<unsigned char>(properties.uuid.bytes[i]));
            visible += byte;
        }
        check(cudaSetDevice(device));
        void* memory = nullptr;
        check(cudaMalloc(&memory, 4096));
        check(cudaMemset(memory, 0, 4096));
        check(cudaDeviceSynchronize());
        check(cudaFree(memory));
    }
    std::printf("allocated=%s\nvisible=%s\n", allocation, visible.c_str());
    return visible == allocation ? 0 : 1;
}
