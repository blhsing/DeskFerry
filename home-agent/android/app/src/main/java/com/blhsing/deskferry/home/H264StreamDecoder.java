package com.blhsing.deskferry.home;

import android.graphics.Bitmap;
import android.graphics.ImageFormat;
import android.graphics.Rect;
import android.media.Image;
import android.media.MediaCodec;
import android.media.MediaCodecInfo;
import android.media.MediaCodecList;
import android.media.MediaFormat;
import android.os.Build;
import android.os.SystemClock;
import android.util.Log;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.util.ArrayDeque;

/**
 * Decodes the H.264 rects (encoding 4) of one live screen session with MediaCodec. Not thread
 * safe: the session thread owns it and feeds the access units in order. Each stream gets its own
 * codec; pictures are converted to ARGB bitmaps from a small per-stream pool that the main thread
 * hands back once it has drawn them.
 */
final class H264StreamDecoder {
    private static final String LOG_TAG = "DeskFerryScreen";
    static final String MIME = "video/avc";
    /** Longest a unit may wait for an input buffer or its picture. */
    private static final long BUDGET_US = 20_000;
    private static final long FRAME_US = 33_333;
    private static final int MAX_POOLED = 3;

    private static final Object PROBE_LOCK = new Object();
    private static boolean probed;
    private static String decoderName;
    private static boolean lowLatencySupported;

    private final MediaCodec.BufferInfo info = new MediaCodec.BufferInfo();
    private final VideoFrames.Scratch scratch = new VideoFrames.Scratch();
    private MediaCodec codec;
    private MediaFormat outputFormat;
    private int width;
    private int height;
    private long frames;
    private int[] pixels = new int[0];
    private BitmapPool pool;

    /** Whether the device has an H.264 decoder for screen-sized pictures; probed once. */
    static boolean available() {
        synchronized (PROBE_LOCK) {
            if (!probed) {
                probed = true;
                try {
                    MediaCodecList list = new MediaCodecList(MediaCodecList.REGULAR_CODECS);
                    decoderName = list.findDecoderForFormat(MediaFormat.createVideoFormat(MIME, 1920, 1088));
                    if (decoderName != null && Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                        for (MediaCodecInfo codecInfo : list.getCodecInfos()) {
                            if (codecInfo.isEncoder() || !decoderName.equals(codecInfo.getName())) continue;
                            lowLatencySupported = codecInfo.getCapabilitiesForType(MIME)
                                    .isFeatureSupported(MediaCodecInfo.CodecCapabilities.FEATURE_LowLatency);
                            break;
                        }
                    }
                } catch (Exception ex) {
                    decoderName = null;
                    Log.w(LOG_TAG, "H.264 decoder probe failed: " + ex);
                }
                Log.i(LOG_TAG, "H.264 decoder: " + (decoderName == null ? "none" : decoderName)
                        + (lowLatencySupported ? " (low latency)" : ""));
            }
            return decoderName != null;
        }
    }

    boolean active() {
        return codec != null;
    }

    boolean matches(int w, int h) {
        return codec != null && width == w && height == h;
    }

    /**
     * Opens a codec for a new stream of {@code w x h} pictures. {@code unit} is the stream's
     * first access unit; its SPS and PPS become the codec-specific data.
     */
    void open(int w, int h, byte[] unit, int offset, int length) throws IOException {
        release();
        if (!available()) throw new IOException("no H.264 decoder");
        int codedWidth = VideoFrames.codedSize(w);
        int codedHeight = VideoFrames.codedSize(h);
        MediaFormat format = baseFormat(codedWidth, codedHeight, unit, offset, length);
        tune(format);
        MediaCodec created = null;
        try {
            created = createCodec();
            try {
                created.configure(format, null, null, 0);
            } catch (Exception tuned) {
                // Some codecs reject the latency hints; retry with the plain format.
                Log.w(LOG_TAG, "H.264 decoder rejected the low-latency format: " + tuned);
                created.release();
                created = createCodec();
                created.configure(baseFormat(codedWidth, codedHeight, unit, offset, length), null, null, 0);
            }
            created.start();
        } catch (Exception ex) {
            if (created != null) {
                try {
                    created.release();
                } catch (Exception ignored) {
                }
            }
            throw new IOException("H.264 decoder could not start: " + ex, ex);
        }
        codec = created;
        outputFormat = null;
        width = w;
        height = h;
        frames = 0;
        if (pool == null || !pool.fits(w, h)) {
            if (pool != null) pool.close();
            pool = new BitmapPool(w, h);
        }
    }

    private static MediaCodec createCodec() throws IOException {
        String name;
        synchronized (PROBE_LOCK) {
            name = decoderName;
        }
        if (name != null) {
            try {
                return MediaCodec.createByCodecName(name);
            } catch (Exception ex) {
                Log.w(LOG_TAG, "H.264 decoder " + name + " unavailable: " + ex);
            }
        }
        return MediaCodec.createDecoderByType(MIME);
    }

    private static MediaFormat baseFormat(int codedWidth, int codedHeight, byte[] unit, int offset, int length) {
        MediaFormat format = MediaFormat.createVideoFormat(MIME, codedWidth, codedHeight);
        format.setInteger(MediaFormat.KEY_COLOR_FORMAT, MediaCodecInfo.CodecCapabilities.COLOR_FormatYUV420Flexible);
        format.setInteger(MediaFormat.KEY_MAX_INPUT_SIZE,
                Math.min(LiveScreenProtocol.MAX_RECORD_PLAIN, Math.max(256 * 1024, codedWidth * codedHeight * 3 / 2)));
        byte[] sps = VideoFrames.nalWithStartCode(unit, offset, length, VideoFrames.NAL_SPS);
        byte[] pps = VideoFrames.nalWithStartCode(unit, offset, length, VideoFrames.NAL_PPS);
        if (sps != null && pps != null) {
            format.setByteBuffer("csd-0", ByteBuffer.wrap(sps));
            format.setByteBuffer("csd-1", ByteBuffer.wrap(pps));
        }
        return format;
    }

    private static void tune(MediaFormat format) {
        boolean lowLatency;
        synchronized (PROBE_LOCK) {
            lowLatency = lowLatencySupported;
        }
        if (lowLatency && Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            format.setInteger(MediaFormat.KEY_LOW_LATENCY, 1);
        }
        format.setInteger(MediaFormat.KEY_PRIORITY, 0);
        format.setInteger(MediaFormat.KEY_OPERATING_RATE, 120);
    }

    /**
     * Feeds one access unit and returns the newest picture the codec produced within the time
     * budget as a pooled {@code w x h} bitmap, or null when none is ready yet. Throws when the codec
     * fails; the caller then releases this stream.
     */
    Bitmap decode(byte[] unit, int offset, int length) throws IOException {
        if (codec == null) return null;
        try {
            long deadline = SystemClock.elapsedRealtimeNanos() / 1000 + BUDGET_US;
            boolean picture = false;
            int input = codec.dequeueInputBuffer(BUDGET_US / 2);
            if (input < 0) {
                picture = drain(deadline, true);
                input = codec.dequeueInputBuffer(Math.max(0, deadline - SystemClock.elapsedRealtimeNanos() / 1000));
                if (input < 0) throw new IOException("H.264 decoder has no free input buffer");
            }
            ByteBuffer buffer = codec.getInputBuffer(input);
            if (buffer == null) throw new IOException("H.264 decoder input buffer is missing");
            buffer.clear();
            if (length > buffer.remaining()) throw new IOException("H.264 access unit is larger than the decoder input");
            buffer.put(unit, offset, length);
            codec.queueInputBuffer(input, 0, length, frames++ * FRAME_US, 0);
            picture |= drain(deadline, false);
            if (!picture) return null;
            Bitmap bitmap = pool.obtain();
            bitmap.setPixels(pixels, 0, width, 0, 0, width, height);
            return bitmap;
        } catch (IOException ex) {
            throw ex;
        } catch (Exception ex) {
            throw new IOException("H.264 decoding failed: " + ex, ex);
        }
    }

    /**
     * Drains output pictures into {@link #pixels}; waits for the first one until {@code deadline}
     * unless {@code noWait}. Returns whether a picture was converted.
     */
    private boolean drain(long deadline, boolean noWait) {
        boolean converted = false;
        for (int guard = 0; guard < 32; guard++) {
            long wait = converted || noWait ? 0 : Math.max(0, deadline - SystemClock.elapsedRealtimeNanos() / 1000);
            int index = codec.dequeueOutputBuffer(info, wait);
            if (index == MediaCodec.INFO_TRY_AGAIN_LATER) break;
            if (index == MediaCodec.INFO_OUTPUT_FORMAT_CHANGED) {
                outputFormat = codec.getOutputFormat();
                continue;
            }
            if (index < 0) continue; // INFO_OUTPUT_BUFFERS_CHANGED and future codes.
            try {
                if (info.size > 0 && convert(index)) converted = true;
            } finally {
                codec.releaseOutputBuffer(index, false);
            }
            if ((info.flags & MediaCodec.BUFFER_FLAG_END_OF_STREAM) != 0) break;
        }
        return converted;
    }

    /** Converts output buffer {@code index} into {@link #pixels}; false when it cannot be read. */
    private boolean convert(int index) {
        if (pixels.length < width * height) pixels = new int[width * height];
        try {
            Image image = codec.getOutputImage(index);
            if (image != null) {
                try {
                    Image.Plane[] planes = image.getPlanes();
                    if (image.getFormat() != ImageFormat.YUV_420_888 || planes == null || planes.length < 3) return false;
                    Rect crop = image.getCropRect();
                    int left = crop == null ? 0 : Math.max(0, crop.left);
                    int top = crop == null ? 0 : Math.max(0, crop.top);
                    if (left + width > image.getWidth() || top + height > image.getHeight()) return false;
                    VideoFrames.yuv420ToArgb(plane(planes[0]), plane(planes[1]), plane(planes[2]),
                            left, top, width, height, pixels, scratch);
                    return true;
                } finally {
                    image.close();
                }
            }
            // No Image view of this buffer: read the raw buffer in its classic colour format.
            MediaFormat format = outputFormat != null ? outputFormat : codec.getOutputFormat();
            ByteBuffer raw = codec.getOutputBuffer(index);
            if (raw == null || format == null || !format.containsKey(MediaFormat.KEY_COLOR_FORMAT)) return false;
            int stride = intKey(format, "stride", intKey(format, MediaFormat.KEY_WIDTH, 0));
            int sliceHeight = intKey(format, "slice-height", intKey(format, MediaFormat.KEY_HEIGHT, 0));
            int left = intKey(format, "crop-left", 0);
            int top = intKey(format, "crop-top", 0);
            ByteBuffer picture = raw.duplicate();
            picture.limit(info.offset + info.size);
            picture.position(info.offset);
            VideoFrames.Plane[] planes = VideoFrames.planarLayout(picture.slice(),
                    format.getInteger(MediaFormat.KEY_COLOR_FORMAT), stride, sliceHeight);
            if (planes == null || left + width > stride || top + height > sliceHeight) return false;
            VideoFrames.yuv420ToArgb(planes[0], planes[1], planes[2], left, top, width, height, pixels, scratch);
            return true;
        } catch (RuntimeException ex) {
            Log.w(LOG_TAG, "H.264 picture conversion failed: " + ex);
            return false;
        }
    }

    private static VideoFrames.Plane plane(Image.Plane plane) {
        return new VideoFrames.Plane(plane.getBuffer(), plane.getRowStride(), plane.getPixelStride());
    }

    private static int intKey(MediaFormat format, String key, int fallback) {
        try {
            return format.containsKey(key) ? format.getInteger(key) : fallback;
        } catch (Exception ex) {
            return fallback;
        }
    }

    /** Bitmap pool of the current stream size, for the DrawOps of its pictures. */
    BitmapPool pool() {
        return pool;
    }

    /** Releases the codec (the pool stays for a following stream of the same size). */
    void release() {
        MediaCodec value = codec;
        codec = null;
        outputFormat = null;
        if (value == null) return;
        try {
            value.stop();
        } catch (Exception ignored) {
        }
        try {
            value.release();
        } catch (Exception ignored) {
        }
    }

    /** Releases the codec and the pool; bitmaps still queued for drawing are recycled when returned. */
    void close() {
        release();
        if (pool != null) pool.close();
        pool = null;
        pixels = new int[0];
    }

    /**
     * Bitmaps of one size, obtained on the session thread and returned by the main thread after
     * drawing. A closed pool recycles whatever comes back.
     */
    static final class BitmapPool {
        private final int width;
        private final int height;
        private final ArrayDeque<Bitmap> free = new ArrayDeque<>();
        private boolean closed;

        BitmapPool(int width, int height) {
            this.width = width;
            this.height = height;
        }

        boolean fits(int w, int h) {
            return width == w && height == h;
        }

        synchronized Bitmap obtain() {
            Bitmap bitmap = free.poll();
            if (bitmap != null) return bitmap;
            bitmap = Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888);
            bitmap.setDensity(Bitmap.DENSITY_NONE);
            return bitmap;
        }

        synchronized void release(Bitmap bitmap) {
            if (bitmap == null || bitmap.isRecycled()) return;
            if (closed || free.size() >= MAX_POOLED || bitmap.getWidth() != width || bitmap.getHeight() != height) {
                bitmap.recycle();
            } else {
                free.push(bitmap);
            }
        }

        synchronized void close() {
            closed = true;
            for (Bitmap bitmap : free) bitmap.recycle();
            free.clear();
        }
    }
}
