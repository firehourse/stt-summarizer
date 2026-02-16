const amqp = require('amqplib');
const fs = require('fs');
const { v4: uuidv4 } = require('uuid');

async function testWorkflow() {
  console.log('🚀 Starting System Integration Test...');
  
  try {
    // 1. Mock Database entry if needed, but here we test the real Gateway logic
    // We simulate a multipart upload or call the API directly? 
    // Let's do a logic check for the critical path: Queue & PubSub
    
    const rabbitURL = `amqp://guest:guest@localhost:5672`;
    const connection = await amqp.connect(rabbitURL);
    const channel = await connection.createChannel();
    
    const taskId = uuidv4();
    const testTask = {
      taskId,
      filePath: '/app/uploads/test.mp3',
      config: { language: 'zh-TW' }
    };

    console.log(`[1] Sending Test Task to RabbitMQ: ${taskId}`);
    await channel.assertQueue('tasks', { durable: true });
    channel.sendToQueue('tasks', Buffer.from(JSON.stringify(testTask)));

    console.log('[2] Task sent. You should see the Go Worker pick it up if running.');
    
    // 2. Cancellation Test Logic
    console.log('[3] Testing Cancellation Signal...');
    const Redis = require('ioredis');
    const redis = new Redis({ host: 'localhost', port: 6379 });
    
    setTimeout(async () => {
      console.log(`[4] Publishing Cancel for ${taskId}`);
      await redis.publish('cancel_channel', JSON.stringify({ taskId }));
      console.log('✅ Cancel signal published.');
      process.exit(0);
    }, 2000);

  } catch (err) {
    console.error('❌ Test failed:', err);
    process.exit(1);
  }
}

testWorkflow();
