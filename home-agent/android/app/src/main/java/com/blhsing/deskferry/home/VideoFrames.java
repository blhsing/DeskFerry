package com.blhsing.deskferry.home;

import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.List;

/**
 * Pure-Java helpers for the H.264 rects of the live screen protocol (encoding 4): Annex B NAL
 * scanning and YUV 4:2:0 to ARGB conversion. Kept free of Android APIs so they can be unit tested
 * on the JVM; {@link H264StreamDecoder} feeds them from MediaCodec output.
 */
final class VideoFrames {
    static final int NAL_SLICE = 1;
    static final int NAL_IDR = 5;
    static final int NAL_SPS = 7;
    static final int NAL_PPS = 8;

    /** MediaCodecInfo.CodecCapabilities.COLOR_FormatYUV420Planar (I420). */
    static final int COLOR_YUV420_PLANAR = 19;
    /** MediaCodecInfo.CodecCapabilities.COLOR_FormatYUV420SemiPlanar (NV12). */
    static final int COLOR_YUV420_SEMI_PLANAR = 21;

    private VideoFrames() {
    }

    /** The coded picture size of a rect side: rounded up to a multiple of 16. */
    static int codedSize(int value) {
        return (value + 15) & ~15;
    }

    /** One NAL unit of an Annex B buffer, without its start code. */
    static final class Nal {
        final int type;
        final int offset;
        final int length;

        Nal(int type, int offset, int length) {
            this.type = type;
            this.offset = offset;
            this.length = length;
        }
    }

    /**
     * Splits an Annex B byte stream (3- or 4-byte start codes) into NAL units. Bytes before the
     * first start code are ignored; trailing zero bytes of a unit (the leading zero of a 4-byte
     * start code, or trailing_zero_8bits) are not part of it.
     */
    static List<Nal> nalUnits(byte[] data, int offset, int length) {
        ArrayList<Nal> units = new ArrayList<>();
        int end = offset + length;
        int start = -1;
        int i = offset;
        while (i + 2 < end) {
            if (data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 1) {
                if (start >= 0) addNal(units, data, start, i);
                start = i + 3;
                i += 3;
            } else {
                i++;
            }
        }
        if (start >= 0) addNal(units, data, start, end);
        return units;
    }

    private static void addNal(List<Nal> units, byte[] data, int start, int end) {
        while (end > start && data[end - 1] == 0) end--;
        if (end > start) units.add(new Nal(data[start] & 0x1F, start, end - start));
    }

    /** The first NAL unit of {@code type} with a 4-byte start code prepended, or null. */
    static byte[] nalWithStartCode(byte[] data, int offset, int length, int type) {
        for (Nal nal : nalUnits(data, offset, length)) {
            if (nal.type != type) continue;
            byte[] out = new byte[4 + nal.length];
            out[3] = 1;
            System.arraycopy(data, nal.offset, out, 4, nal.length);
            return out;
        }
        return null;
    }

    static boolean containsNal(byte[] data, int offset, int length, int type) {
        for (Nal nal : nalUnits(data, offset, length)) {
            if (nal.type == type) return true;
        }
        return false;
    }

    // ---- YUV 4:2:0 to ARGB -----------------------------------------------------------------

    /** One plane of a YUV 4:2:0 picture as MediaCodec's Image.Plane describes it. */
    static final class Plane {
        final ByteBuffer buffer;
        final int rowStride;
        final int pixelStride;

        Plane(ByteBuffer buffer, int rowStride, int pixelStride) {
            this.buffer = buffer;
            this.rowStride = rowStride;
            this.pixelStride = pixelStride;
        }
    }

    /** Reusable row buffers for {@link #yuv420ToArgb}; not thread safe. */
    static final class Scratch {
        byte[] luma = new byte[0];
        byte[] u = new byte[0];
        byte[] v = new byte[0];

        private static byte[] ensure(byte[] value, int size) {
            return value.length >= size ? value : new byte[size];
        }
    }

    /**
     * Converts the {@code width x height} area at ({@code left}, {@code top}) of a YUV 4:2:0
     * picture (any planar or semi-planar layout, described by row and pixel strides) to opaque
     * ARGB pixels in {@code out} (row stride {@code width}), with BT.601 limited-range
     * coefficients as the Work PC's encoder uses. Buffer positions are changed; limits are not.
     */
    static void yuv420ToArgb(Plane y, Plane u, Plane v, int left, int top, int width, int height,
                             int[] out, Scratch scratch) {
        if (width <= 0 || height <= 0) return;
        if (out.length < width * height) throw new IllegalArgumentException("output is too small");
        int lumaBytes = (width - 1) * y.pixelStride + 1;
        int chromaLeft = left >> 1;
        int chromaCount = ((left + width - 1) >> 1) - chromaLeft + 1;
        int uBytes = (chromaCount - 1) * u.pixelStride + 1;
        int vBytes = (chromaCount - 1) * v.pixelStride + 1;
        byte[] lrow = scratch.luma = Scratch.ensure(scratch.luma, lumaBytes);
        byte[] urow = scratch.u = Scratch.ensure(scratch.u, uBytes);
        byte[] vrow = scratch.v = Scratch.ensure(scratch.v, vBytes);
        int lumaStep = y.pixelStride;
        int uStep = u.pixelStride;
        int vStep = v.pixelStride;
        int loadedChromaRow = -1;
        for (int row = 0; row < height; row++) {
            int sy = top + row;
            read(y.buffer, sy * y.rowStride + left * lumaStep, lrow, lumaBytes);
            int cy = sy >> 1;
            if (cy != loadedChromaRow) {
                read(u.buffer, cy * u.rowStride + chromaLeft * uStep, urow, uBytes);
                read(v.buffer, cy * v.rowStride + chromaLeft * vStep, vrow, vBytes);
                loadedChromaRow = cy;
            }
            int target = row * width;
            for (int col = 0; col < width; col++) {
                int c = 298 * ((lrow[col * lumaStep] & 0xFF) - 16);
                int chroma = ((left + col) >> 1) - chromaLeft;
                int d = (urow[chroma * uStep] & 0xFF) - 128;
                int e = (vrow[chroma * vStep] & 0xFF) - 128;
                int r = clamp((c + 409 * e + 128) >> 8);
                int g = clamp((c - 100 * d - 208 * e + 128) >> 8);
                int b = clamp((c + 516 * d + 128) >> 8);
                out[target + col] = 0xFF000000 | r << 16 | g << 8 | b;
            }
        }
    }

    private static void read(ByteBuffer buffer, int position, byte[] target, int count) {
        if (position < 0 || position + count > buffer.limit()) {
            throw new IndexOutOfBoundsException("picture plane is smaller than its strides describe");
        }
        buffer.position(position);
        buffer.get(target, 0, count);
    }

    private static int clamp(int value) {
        return value < 0 ? 0 : (value > 255 ? 255 : value);
    }

    /**
     * Describes the planes of a raw MediaCodec output buffer in a classic YUV 4:2:0 colour format
     * (I420 or NV12) with the given luma stride and slice height; returns {y, u, v} or null for
     * an unsupported format. {@code buffer} holds the picture from position 0.
     */
    static Plane[] planarLayout(ByteBuffer buffer, int colorFormat, int stride, int sliceHeight) {
        int lumaSize = stride * sliceHeight;
        if (stride <= 0 || sliceHeight <= 0 || lumaSize > buffer.limit()) return null;
        if (colorFormat == COLOR_YUV420_PLANAR) {
            int chromaStride = (stride + 1) / 2;
            int chromaSize = chromaStride * ((sliceHeight + 1) / 2);
            return new Plane[]{
                    new Plane(slice(buffer, 0, buffer.limit()), stride, 1),
                    new Plane(slice(buffer, lumaSize, buffer.limit()), chromaStride, 1),
                    new Plane(slice(buffer, lumaSize + chromaSize, buffer.limit()), chromaStride, 1),
            };
        }
        if (colorFormat == COLOR_YUV420_SEMI_PLANAR) {
            return new Plane[]{
                    new Plane(slice(buffer, 0, buffer.limit()), stride, 1),
                    new Plane(slice(buffer, lumaSize, buffer.limit()), stride, 2),
                    new Plane(slice(buffer, lumaSize + 1, buffer.limit()), stride, 2),
            };
        }
        return null;
    }

    private static ByteBuffer slice(ByteBuffer buffer, int start, int limit) {
        ByteBuffer copy = buffer.duplicate();
        if (start > limit) start = limit;
        copy.limit(limit);
        copy.position(start);
        return copy.slice();
    }
}
