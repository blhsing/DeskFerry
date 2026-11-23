package com.blhsing.deskferry.home;

import org.junit.Test;

import java.nio.ByteBuffer;
import java.util.Arrays;
import java.util.List;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

public class VideoFramesTest {
    @Test
    public void roundsCodedSizeTo16() {
        assertEquals(16, VideoFrames.codedSize(1));
        assertEquals(16, VideoFrames.codedSize(16));
        assertEquals(1088, VideoFrames.codedSize(1080));
        assertEquals(1920, VideoFrames.codedSize(1920));
    }

    @Test
    public void scansAnnexBNalUnits() {
        byte[] unit = new byte[]{
                0x09, // garbage before the first start code is ignored
                0, 0, 0, 1, 0x67, 0x42, (byte) 0xC0, 0x1F,
                0, 0, 1, 0x68, (byte) 0xCE, 0x3C, (byte) 0x80,
                0, 0, 0, 1, 0x65, (byte) 0x88, (byte) 0x84, 0, 0,
        };
        List<VideoFrames.Nal> nals = VideoFrames.nalUnits(unit, 0, unit.length);
        assertEquals(3, nals.size());
        assertEquals(VideoFrames.NAL_SPS, nals.get(0).type);
        assertEquals(5, nals.get(0).offset);
        assertEquals(4, nals.get(0).length);
        assertEquals(VideoFrames.NAL_PPS, nals.get(1).type);
        assertEquals(4, nals.get(1).length);
        assertEquals(VideoFrames.NAL_IDR, nals.get(2).type);
        assertEquals(3, nals.get(2).length); // trailing zero bytes are not part of the unit
        assertTrue(VideoFrames.containsNal(unit, 0, unit.length, VideoFrames.NAL_IDR));
        assertFalse(VideoFrames.containsNal(unit, 0, unit.length, VideoFrames.NAL_SLICE));
        assertArrayEquals(new byte[]{0, 0, 0, 1, 0x67, 0x42, (byte) 0xC0, 0x1F},
                VideoFrames.nalWithStartCode(unit, 0, unit.length, VideoFrames.NAL_SPS));
        assertArrayEquals(new byte[]{0, 0, 0, 1, 0x68, (byte) 0xCE, 0x3C, (byte) 0x80},
                VideoFrames.nalWithStartCode(unit, 0, unit.length, VideoFrames.NAL_PPS));
        assertNull(VideoFrames.nalWithStartCode(unit, 0, unit.length, 6));
        assertTrue(VideoFrames.nalUnits(new byte[]{1, 2, 3}, 0, 3).isEmpty());
        // A slice of a larger array.
        byte[] padded = new byte[unit.length + 4];
        System.arraycopy(unit, 0, padded, 2, unit.length);
        assertEquals(3, VideoFrames.nalUnits(padded, 2, unit.length).size());
    }

    private static int argb(int y, int u, int v) {
        // Reference BT.601 limited-range conversion in floating point.
        double c = 1.164383 * (y - 16);
        int r = clamp(Math.round(c + 1.596027 * (v - 128)));
        int g = clamp(Math.round(c - 0.391762 * (u - 128) - 0.812968 * (v - 128)));
        int b = clamp(Math.round(c + 2.017232 * (u - 128)));
        return 0xFF000000 | r << 16 | g << 8 | b;
    }

    private static int clamp(long value) {
        return (int) Math.max(0, Math.min(255, value));
    }

    private static void assertClose(int expected, int actual) {
        for (int shift = 0; shift < 32; shift += 8) {
            int e = (expected >>> shift) & 0xFF;
            int a = (actual >>> shift) & 0xFF;
            assertTrue("channel " + shift + " expected " + Integer.toHexString(expected) + " got " + Integer.toHexString(actual),
                    Math.abs(e - a) <= 1);
        }
    }

    @Test
    public void convertsKnownColours() {
        // 2x2 picture, one chroma sample: black, white, then BT.601 red, green and blue.
        int[][] samples = {{16, 128, 128}, {235, 128, 128}, {81, 90, 240}, {145, 54, 34}, {41, 240, 110}};
        int[] expected = {0xFF000000, 0xFFFFFFFF, 0xFFFF0000, 0xFF00FF00, 0xFF0000FF};
        for (int i = 0; i < samples.length; i++) {
            ByteBuffer y = ByteBuffer.wrap(new byte[]{(byte) samples[i][0], (byte) samples[i][0], (byte) samples[i][0], (byte) samples[i][0]});
            ByteBuffer u = ByteBuffer.wrap(new byte[]{(byte) samples[i][1]});
            ByteBuffer v = ByteBuffer.wrap(new byte[]{(byte) samples[i][2]});
            int[] out = new int[4];
            VideoFrames.yuv420ToArgb(new VideoFrames.Plane(y, 2, 1), new VideoFrames.Plane(u, 1, 1),
                    new VideoFrames.Plane(v, 1, 1), 0, 0, 2, 2, out, new VideoFrames.Scratch());
            for (int pixel : out) assertClose(expected[i], pixel);
        }
    }

    /** Pseudo-random plane bytes in a plausible range. */
    private static byte[] fill(int size, int seed) {
        byte[] out = new byte[size];
        int state = seed;
        for (int i = 0; i < size; i++) {
            state = state * 1103515245 + 12345;
            out[i] = (byte) (16 + ((state >>> 16) % 225));
        }
        return out;
    }

    @Test
    public void convertsPlanarWithStridesAndCrop() {
        int codedWidth = 8;
        int codedHeight = 6;
        int lumaStride = 12;
        int chromaStride = 7;
        // Tight buffers: the last row of each plane is shorter than its stride.
        byte[] luma = fill(lumaStride * (codedHeight - 1) + codedWidth, 1);
        byte[] cb = fill(chromaStride * (codedHeight / 2 - 1) + codedWidth / 2, 2);
        byte[] cr = fill(chromaStride * (codedHeight / 2 - 1) + codedWidth / 2, 3);
        int left = 1;
        int top = 1;
        int width = 7;
        int height = 5;
        int[] out = new int[width * height];
        VideoFrames.yuv420ToArgb(new VideoFrames.Plane(ByteBuffer.wrap(luma), lumaStride, 1),
                new VideoFrames.Plane(ByteBuffer.wrap(cb), chromaStride, 1),
                new VideoFrames.Plane(ByteBuffer.wrap(cr), chromaStride, 1),
                left, top, width, height, out, new VideoFrames.Scratch());
        for (int row = 0; row < height; row++) {
            for (int col = 0; col < width; col++) {
                int sx = left + col;
                int sy = top + row;
                int expected = argb(luma[sy * lumaStride + sx] & 0xFF,
                        cb[(sy / 2) * chromaStride + sx / 2] & 0xFF, cr[(sy / 2) * chromaStride + sx / 2] & 0xFF);
                assertClose(expected, out[row * width + col]);
            }
        }
    }

    @Test
    public void convertsSemiPlanarImagePlanes() {
        // NV12 as Image planes: U and V share one interleaved buffer with pixel stride 2.
        int width = 6;
        int height = 4;
        int stride = 8;
        byte[] luma = fill(stride * height, 4);
        byte[] chroma = fill(stride * (height / 2), 5);
        ByteBuffer u = ByteBuffer.wrap(chroma, 0, stride * (height / 2) - 1).slice();
        ByteBuffer v = ByteBuffer.wrap(chroma, 1, stride * (height / 2) - 1).slice();
        int[] out = new int[width * height];
        VideoFrames.Scratch scratch = new VideoFrames.Scratch();
        VideoFrames.yuv420ToArgb(new VideoFrames.Plane(ByteBuffer.wrap(luma), stride, 1),
                new VideoFrames.Plane(u, stride, 2), new VideoFrames.Plane(v, stride, 2),
                0, 0, width, height, out, scratch);
        int[] expected = new int[width * height];
        for (int row = 0; row < height; row++) {
            for (int col = 0; col < width; col++) {
                int c = (row / 2) * stride + (col / 2) * 2;
                expected[row * width + col] = argb(luma[row * stride + col] & 0xFF, chroma[c] & 0xFF, chroma[c + 1] & 0xFF);
                assertClose(expected[row * width + col], out[row * width + col]);
            }
        }

        // The same picture through the raw-buffer layout of COLOR_FormatYUV420SemiPlanar.
        byte[] raw = new byte[stride * height + stride * (height / 2)];
        System.arraycopy(luma, 0, raw, 0, luma.length);
        System.arraycopy(chroma, 0, raw, luma.length, chroma.length);
        VideoFrames.Plane[] planes = VideoFrames.planarLayout(ByteBuffer.wrap(raw), VideoFrames.COLOR_YUV420_SEMI_PLANAR, stride, height);
        int[] again = new int[width * height];
        VideoFrames.yuv420ToArgb(planes[0], planes[1], planes[2], 0, 0, width, height, again, scratch);
        assertArrayEquals(out, again);
    }

    @Test
    public void readsRawPlanarLayout() {
        int width = 4;
        int height = 4;
        int stride = 6;
        int sliceHeight = 4;
        byte[] luma = fill(stride * sliceHeight, 6);
        byte[] cb = fill(3 * 2, 7);
        byte[] cr = fill(3 * 2, 8);
        byte[] raw = new byte[luma.length + cb.length + cr.length];
        System.arraycopy(luma, 0, raw, 0, luma.length);
        System.arraycopy(cb, 0, raw, luma.length, cb.length);
        System.arraycopy(cr, 0, raw, luma.length + cb.length, cr.length);
        VideoFrames.Plane[] planes = VideoFrames.planarLayout(ByteBuffer.wrap(raw), VideoFrames.COLOR_YUV420_PLANAR, stride, sliceHeight);
        int[] out = new int[width * height];
        VideoFrames.yuv420ToArgb(planes[0], planes[1], planes[2], 0, 0, width, height, out, new VideoFrames.Scratch());
        for (int row = 0; row < height; row++) {
            for (int col = 0; col < width; col++) {
                int c = (row / 2) * 3 + col / 2;
                assertClose(argb(luma[row * stride + col] & 0xFF, cb[c] & 0xFF, cr[c] & 0xFF), out[row * width + col]);
            }
        }
        assertNull(VideoFrames.planarLayout(ByteBuffer.wrap(raw), 0x7FA30C04, stride, sliceHeight));
        assertNull(VideoFrames.planarLayout(ByteBuffer.wrap(new byte[4]), VideoFrames.COLOR_YUV420_PLANAR, stride, sliceHeight));
        assertTrue(Arrays.stream(out).allMatch(pixel -> (pixel >>> 24) == 0xFF));
    }
}
