using System.Buffers;
using System.Net.WebSockets;
using System.Runtime.ExceptionServices;
using System.Threading.Channels;

/// <summary>
/// Reads a resume attachment continuously so the relay notices when a side
/// abandons a waiting resume socket and tries another relay. Frames are replayed
/// to the bridge in order, with their original message boundaries.
/// </summary>
sealed class ReadAheadWebSocket : WebSocket
{
    private const int ChunkSize = 64 * 1024;

    private sealed record Frame(byte[] Data, int Count, WebSocketMessageType Type, bool EndOfMessage, WebSocketCloseStatus? CloseStatus, string? CloseDescription, Exception? Error);

    private readonly WebSocket _inner;
    private readonly Channel<Frame> _frames = Channel.CreateBounded<Frame>(new BoundedChannelOptions(64) { SingleReader = true, SingleWriter = true });
    private readonly CancellationTokenSource _stop = new();
    private readonly TaskCompletionSource _dead = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private Frame? _partial;
    private int _partialOffset;

    public ReadAheadWebSocket(WebSocket inner)
    {
        _inner = inner;
        _ = Task.Run(ReadLoopAsync);
    }

    public bool IsDead => _dead.Task.IsCompleted;

    private async Task ReadLoopAsync()
    {
        try
        {
            while (true)
            {
                var buffer = ArrayPool<byte>.Shared.Rent(ChunkSize);
                WebSocketReceiveResult received;
                try
                {
                    received = await _inner.ReceiveAsync(new ArraySegment<byte>(buffer, 0, ChunkSize), _stop.Token);
                }
                catch (Exception ex)
                {
                    ArrayPool<byte>.Shared.Return(buffer);
                    _dead.TrySetResult();
                    _frames.Writer.TryWrite(new Frame(Array.Empty<byte>(), 0, WebSocketMessageType.Close, true, null, null, ex));
                    return;
                }
                if (received.MessageType == WebSocketMessageType.Close)
                {
                    ArrayPool<byte>.Shared.Return(buffer);
                    _dead.TrySetResult();
                    _frames.Writer.TryWrite(new Frame(Array.Empty<byte>(), 0, WebSocketMessageType.Close, true, received.CloseStatus, received.CloseStatusDescription, null));
                    return;
                }
                await _frames.Writer.WriteAsync(new Frame(buffer, received.Count, received.MessageType, received.EndOfMessage, null, null, null), _stop.Token);
            }
        }
        catch (OperationCanceledException)
        {
            _dead.TrySetResult();
        }
        finally
        {
            _frames.Writer.TryComplete();
        }
    }

    public override async Task<WebSocketReceiveResult> ReceiveAsync(ArraySegment<byte> buffer, CancellationToken cancellationToken)
    {
        if (_partial is null)
        {
            if (!await _frames.Reader.WaitToReadAsync(cancellationToken) || !_frames.Reader.TryRead(out var frame))
            {
                throw new WebSocketException(WebSocketError.InvalidState, "The resume attachment is closed.");
            }
            if (frame.Error is not null)
            {
                ExceptionDispatchInfo.Throw(frame.Error);
            }
            if (frame.Type == WebSocketMessageType.Close)
            {
                return new WebSocketReceiveResult(0, WebSocketMessageType.Close, true, frame.CloseStatus, frame.CloseDescription);
            }
            _partial = frame;
            _partialOffset = 0;
        }
        var current = _partial;
        var count = Math.Min(buffer.Count, current.Count - _partialOffset);
        Buffer.BlockCopy(current.Data, _partialOffset, buffer.Array!, buffer.Offset, count);
        _partialOffset += count;
        var finished = _partialOffset >= current.Count;
        var result = new WebSocketReceiveResult(count, current.Type, finished && current.EndOfMessage);
        if (finished)
        {
            ArrayPool<byte>.Shared.Return(current.Data);
            _partial = null;
        }
        return result;
    }

    public override Task SendAsync(ArraySegment<byte> buffer, WebSocketMessageType messageType, bool endOfMessage, CancellationToken cancellationToken) =>
        _inner.SendAsync(buffer, messageType, endOfMessage, cancellationToken);

    public override ValueTask SendAsync(ReadOnlyMemory<byte> buffer, WebSocketMessageType messageType, bool endOfMessage, CancellationToken cancellationToken) =>
        _inner.SendAsync(buffer, messageType, endOfMessage, cancellationToken);

    public override Task CloseAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken) =>
        _inner.CloseAsync(closeStatus, statusDescription, cancellationToken);

    public override Task CloseOutputAsync(WebSocketCloseStatus closeStatus, string? statusDescription, CancellationToken cancellationToken) =>
        _inner.CloseOutputAsync(closeStatus, statusDescription, cancellationToken);

    public override void Abort()
    {
        _stop.Cancel();
        _inner.Abort();
    }

    public override void Dispose()
    {
        _stop.Cancel();
        _inner.Dispose();
    }

    public override WebSocketCloseStatus? CloseStatus => _inner.CloseStatus;
    public override string? CloseStatusDescription => _inner.CloseStatusDescription;
    public override WebSocketState State => _inner.State;
    public override string? SubProtocol => _inner.SubProtocol;
}
